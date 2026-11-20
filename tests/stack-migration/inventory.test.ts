import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';

export interface InventoryReport {
  valid: boolean;
  commitSha: string;
  duplicates: string[];
  unclassified: string[];
  missingFiles: string[];
  rowCounts: {
    routes: number;
    features: number;
    dataCoverage: number;
  };
}

function parseCsv(filePath: string): Array<Record<string, string>> {
  const raw = fs.readFileSync(filePath, 'utf8').trim();
  if (!raw) return [];
  const lines = raw.split(/\r?\n/);
  const headers = parseCsvLine(lines[0]);
  const rows: Array<Record<string, string>> = [];
  for (let i = 1; i < lines.length; i++) {
    if (!lines[i].trim()) continue;
    const cols = parseCsvLine(lines[i]);
    const row: Record<string, string> = {};
    for (let j = 0; j < headers.length; j++) {
      row[headers[j]] = cols[j] ?? '';
    }
    rows.push(row);
  }
  return rows;
}

function parseCsvLine(line: string): string[] {
  const result: string[] = [];
  let current = '';
  let inQuotes = false;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (inQuotes) {
      if (ch === '"' && line[i + 1] === '"') {
        current += '"';
        i++;
      } else if (ch === '"') {
        inQuotes = false;
      } else {
        current += ch;
      }
    } else if (ch === '"') {
      inQuotes = true;
    } else if (ch === ',') {
      result.push(current);
      current = '';
    } else {
      current += ch;
    }
  }
  result.push(current);
  return result;
}

export function CheckInventoryContract(rootDir: string): InventoryReport {
  const requiredFiles = [
    'migration/baseline.md',
    'migration/features.csv',
    'migration/routes.csv',
    'migration/data-coverage.csv',
    'migration/upstream-tracking.md',
  ];
  const missingFiles = requiredFiles.filter((rel) => !fs.existsSync(path.join(rootDir, rel)));
  if (missingFiles.length > 0) {
    return {
      valid: false,
      commitSha: '',
      duplicates: [],
      unclassified: [],
      missingFiles,
      rowCounts: { routes: 0, features: 0, dataCoverage: 0 },
    };
  }

  const baseline = fs.readFileSync(path.join(rootDir, 'migration/baseline.md'), 'utf8');
  const shaMatch = baseline.match(/\b([0-9a-f]{40})\b/i);
  const commitSha = shaMatch ? shaMatch[1] : '';

  const routes = parseCsv(path.join(rootDir, 'migration/routes.csv'));
  const features = parseCsv(path.join(rootDir, 'migration/features.csv'));
  const dataCov = parseCsv(path.join(rootDir, 'migration/data-coverage.csv'));

  const seenIds = new Set<string>();
  const duplicates: string[] = [];
  const unclassified: string[] = [];

  for (const [name, rows, reqCols] of [
    ['routes', routes, ['id', 'kind', 'method', 'path', 'source', 'target_owner', 'migration_decision', 'status']] as const,
    ['features', features, ['id', 'domain', 'source', 'target_owner', 'acceptance_evidence', 'migration_decision', 'status']] as const,
    ['data-coverage', dataCov, ['id', 'model', 'table_or_scope', 'encrypted_fields', 'sentinel_zero', 'target_owner', 'migration_decision', 'status']] as const,
  ]) {
    for (const row of rows) {
      const id = row.id?.trim();
      if (!id) {
        unclassified.push(`${name}:missing_id`);
        continue;
      }
      if (seenIds.has(id)) {
        duplicates.push(id);
      }
      seenIds.add(id);

      for (const col of reqCols) {
        if (row[col] === undefined || (col !== 'encrypted_fields' && !row[col].trim())) {
          unclassified.push(`${id}:missing_${col}`);
        }
      }
      if (row.target_owner === 'unclassified' || row.migration_decision === 'unclassified') {
        unclassified.push(`${id}:unclassified`);
      }
    }
  }

  // Ensure every Model file in app/Models is covered in data-coverage.csv
  const modelDir = path.join(rootDir, 'app/Models');
  if (fs.existsSync(modelDir)) {
    const modelFiles = fs.readdirSync(modelDir).filter((f) => f.endsWith('.php')).sort();
    const coveredModels = new Set(dataCov.map((r) => r.model));
    for (const mf of modelFiles) {
      const modelName = mf.replace(/\.php$/, '');
      if (!coveredModels.has(modelName)) {
        unclassified.push(`model:${modelName}`);
      }
    }
  }

  // Ensure every Policy file in app/Policies is covered in features.csv
  const policyDir = path.join(rootDir, 'app/Policies');
  if (fs.existsSync(policyDir)) {
    const policyFiles = fs.readdirSync(policyDir).filter((f) => f.endsWith('.php')).sort();
    const coveredSources = new Set(features.map((r) => r.source));
    for (const pf of policyFiles) {
      const rel = `app/Policies/${pf}`;
      if (!coveredSources.has(rel)) {
        unclassified.push(`policy:${rel}`);
      }
    }
  }

  // Ensure every concrete table in data-coverage.csv has matching DDL in backend/internal/store/postgres/
  const schemaPath = path.join(rootDir, 'backend/internal/store/postgres/schema.go');
  const schemaExtPath = path.join(rootDir, 'backend/internal/store/postgres/schema_extended.go');
  if (fs.existsSync(schemaPath) && fs.existsSync(schemaExtPath)) {
    const ddl = fs.readFileSync(schemaPath, 'utf8') + '\n' + fs.readFileSync(schemaExtPath, 'utf8');
    const nonTables = new Set(['abstract_base_model', 'query_scope_trait']);
    for (const row of dataCov) {
      const tbl = (row.table_or_scope || '').trim().split(/\s+/)[0];
      if (!tbl || nonTables.has(tbl)) continue;
      if (!new RegExp(`CREATE\\s+TABLE\\s+IF\\s+NOT\\s+EXISTS\\s+${tbl}\\b`, 'i').test(ddl)) {
        unclassified.push(`schema_missing:${tbl}`);
      }
    }
  }

  const valid =
    missingFiles.length === 0 &&
    commitSha.length === 40 &&
    duplicates.length === 0 &&
    unclassified.length === 0 &&
    routes.length > 0 &&
    features.length > 0 &&
    dataCov.length > 0;

  return {
    valid,
    commitSha,
    duplicates,
    unclassified,
    missingFiles,
    rowCounts: {
      routes: routes.length,
      features: features.length,
      dataCoverage: dataCov.length,
    },
  };
}

describe('Task 1: Migration Baseline & Inventory Contract', () => {
  it('validates complete frozen baseline SHA, zero duplicates, and zero unclassified entries', () => {
    const rootDir = path.resolve(import.meta.dirname, '../..');
    const report = CheckInventoryContract(rootDir);
    assert.deepEqual(report.missingFiles, [], `Missing files: ${report.missingFiles.join(', ')}`);
    assert.match(report.commitSha, /^[0-9a-f]{40}$/i, 'baseline.md must record 40-char commit SHA');
    assert.deepEqual(report.duplicates, [], `Duplicate IDs: ${report.duplicates.join(', ')}`);
    assert.deepEqual(report.unclassified, [], `Unclassified entries: ${report.unclassified.join(', ')}`);
    assert.equal(report.valid, true);
    assert.ok(report.rowCounts.routes >= 600, `Expected >= 600 routes, got ${report.rowCounts.routes}`);
    assert.ok(report.rowCounts.features >= 500, `Expected >= 500 features/components, got ${report.rowCounts.features}`);
    assert.ok(report.rowCounts.dataCoverage >= 81, `Expected >= 81 models, got ${report.rowCounts.dataCoverage}`);
  });
});
