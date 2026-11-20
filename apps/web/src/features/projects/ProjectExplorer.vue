<script setup lang="ts">
import { onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import { usePageLifecycle } from '../../lib/use-page-lifecycle';

export interface ProjectSummary {
  id: number;
  uuid: string;
  name: string;
  description?: string;
}

const emit = defineEmits<{ select: [uuid: string] }>();

const lifecycle = usePageLifecycle();
const projects = ref<ProjectSummary[]>([]);
const starredIds = ref<Set<number>>(new Set());
const newName = ref('');
const newDescription = ref('');
const status = ref('');

async function loadProjects() {
  try {
    const [projList, starList] = await Promise.all([
      apiRequest<ProjectSummary[]>('/api/v1/projects', { signal: lifecycle.signal }),
      apiRequest<Array<{ starrable_type: string; starrable_id: number }>>('/api/v1/stars', { signal: lifecycle.signal }).catch(() => []),
    ]);
    projects.value = projList;
    const set = new Set<number>();
    for (const s of starList) {
      if (s.starrable_type.toLowerCase().includes('project')) set.add(s.starrable_id);
    }
    starredIds.value = set;
  } catch {
    projects.value = [];
  }
}

async function toggleStar(projectId: number) {
  try {
    await apiRequest('/api/v1/stars', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ starrable_type: 'project', starrable_id: projectId, position: 0 }),
    });
    const next = new Set(starredIds.value);
    next.add(projectId);
    starredIds.value = next;
  } catch {
    // Ignore errors
  }
}

async function createProject() {
  if (!newName.value.trim()) return;
  status.value = '';
  try {
    const res = await apiRequest<{ project: ProjectSummary }>('/api/v1/projects', {
      method: 'POST',
      signal: lifecycle.signal,
      body: JSON.stringify({ name: newName.value.trim(), description: newDescription.value.trim() }),
    });
    newName.value = '';
    newDescription.value = '';
    status.value = `Project created: ${res.project.name}`;
    await loadProjects();
    emit('select', res.project.uuid);
  } catch (err) {
    status.value = (err as Error).message;
  }
}

lifecycle.onTeamSwitch(() => {
  projects.value = [];
  status.value = '';
  void loadProjects();
});

onMounted(() => {
  void loadProjects();
});

defineExpose({ refresh: loadProjects });
</script>

<template>
  <div class="coolify-card">
    <div class="coolify-well" style="display:grid;gap:10px;">
      <strong>Projects &amp; Environments</strong>
      <form style="display:flex;gap:8px;flex-wrap:wrap;" @submit.prevent="createProject">
        <input v-model="newName" class="coolify-input" placeholder="New project name" aria-label="New project name" required />
        <input v-model="newDescription" class="coolify-input" placeholder="Description (optional)" aria-label="Project description" />
        <button type="submit" class="coolify-btn coolify-btn-primary">Create Project</button>
      </form>
      <p v-if="status" role="status" style="margin:0;">{{ status }}</p>
      <p v-if="projects.length === 0" style="color:var(--ui-fg-dim);margin:0;">No projects yet.</p>
      <ul v-else style="list-style:none;padding:0;margin:0;display:grid;gap:6px;">
        <li
          v-for="p in projects"
          :key="p.uuid"
          style="display:flex;justify-content:space-between;align-items:center;padding:6px 8px;border:1px solid var(--ui-well-border);border-radius:6px;"
        >
          <a :href="`/project/${p.uuid}`" @click.prevent="emit('select', p.uuid)">{{ p.name }}</a>
          <button type="button" class="coolify-btn" @click="toggleStar(p.id)">
            {{ starredIds.has(p.id) ? 'Starred' : 'Star' }}
          </button>
        </li>
      </ul>
    </div>
  </div>
</template>
