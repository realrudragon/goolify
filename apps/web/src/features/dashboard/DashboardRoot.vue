<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue';
import { apiRequest } from '../../lib/api';
import ServerList from '../servers/ServerList.vue';
import ServerSummary from '../servers/ServerSummary.vue';
import ServerForm from '../servers/ServerForm.vue';
import ServerSettings from '../servers/ServerSettings.vue';
import ServerOpsPanel from '../servers/ServerOpsPanel.vue';
import ProjectExplorer from '../projects/ProjectExplorer.vue';
import ResourcesManager from '../projects/ResourcesManager.vue';
import CanvasGraph from '../canvas/CanvasGraph.vue';
import TeamAccess from '../teams/TeamAccess.vue';
import LogViewer from '../logs/LogViewer.vue';
import WebTerminal from '../terminal/WebTerminal.vue';
import DeployControl from '../deployments/DeployControl.vue';
import DeploymentHistory from '../deployments/DeploymentHistory.vue';
import ServiceEditor from '../services/ServiceEditor.vue';
import DatabasePanel from '../databases/DatabasePanel.vue';
import BackupManager from '../backups/BackupManager.vue';
import ScheduleList from '../schedules/ScheduleList.vue';
import NotificationSettings from '../notifications/NotificationSettings.vue';
import ProxyConfig from '../proxy/ProxyConfig.vue';
import ServerTransferPanel from '../transfer/ServerTransferPanel.vue';
import ApiTokenManager from '../auth/ApiTokenManager.vue';
import SourcesAndCloudPanel from '../integrations/SourcesAndCloudPanel.vue';
import SecurityAndVulnPanel from '../security/SecurityAndVulnPanel.vue';
import GovernanceAndAuthPanel from '../settings/GovernanceAndAuthPanel.vue';

type TabKey = 'overview' | 'servers' | 'projects' | 'operations' | 'databases' | 'sources' | 'security' | 'settings';

const activeTab = ref<TabKey>('overview');
const activeServerUuid = ref('localhost');
const activeResourceUuid = ref('localhost');
const activeApplicationUuid = ref('');
const activeServiceUuid = ref('');
const activeDatabaseUuid = ref('coolify-db');
const activeDatabaseType = ref('postgresql');

const serverListRef = ref<InstanceType<typeof ServerList> | null>(null);
const scheduleListRef = ref<InstanceType<typeof ScheduleList> | null>(null);
const deployHistoryRef = ref<InstanceType<typeof DeploymentHistory> | null>(null);
const resourcesManagerRef = ref<InstanceType<typeof ResourcesManager> | null>(null);

const realtimeToast = ref('');
const showHelp = ref(false);
const helpSubject = ref('');
const helpDescription = ref('');
const helpStatus = ref('');
let eventStream: EventSource | null = null;

function subscribeRealtimeToasts() {
  if (typeof window === 'undefined' || typeof EventSource === 'undefined') return;
  try {
    eventStream = new EventSource('/realtime/events?channel=team.0');
    eventStream.onmessage = (ev) => {
      try {
        const msg = JSON.parse(ev.data) as { mutation_type?: string; resource_uuid?: string };
        if (msg.mutation_type) {
          realtimeToast.value = `Realtime update: ${msg.mutation_type} (${msg.resource_uuid || 'team'})`;
        }
      } catch {
        // Ignore heartbeat frames
      }
    };
  } catch {
    eventStream = null;
  }
}

async function submitHelpFeedback() {
  if (!helpSubject.value.trim() || !helpDescription.value.trim()) return;
  helpStatus.value = '';
  try {
    await apiRequest('/api/v1/feedback', {
      method: 'POST',
      body: JSON.stringify({
        subject: helpSubject.value.trim(),
        description: helpDescription.value.trim(),
      }),
    });
    helpSubject.value = '';
    helpDescription.value = '';
    helpStatus.value = 'Feedback sent.';
  } catch (err) {
    helpStatus.value = (err as Error).message;
  }
}

function resolveTabFromPath() {
  if (typeof window === 'undefined') return;
  const p = window.location.pathname;
  if (p.startsWith('/servers') || p.startsWith('/server/') || p.startsWith('/destinations') || p.startsWith('/destination/')) {
    activeTab.value = 'servers';
    const match = p.match(/^\/server\/([^/]+)/);
    if (match?.[1]) {
      activeServerUuid.value = decodeURIComponent(match[1]);
      activeResourceUuid.value = activeServerUuid.value;
    }
  } else if (p.startsWith('/projects') || p.startsWith('/project/') || p.startsWith('/services') || p.startsWith('/deployments')) {
    activeTab.value = 'projects';
  } else if (p.startsWith('/databases') || p.startsWith('/storages')) {
    activeTab.value = 'databases';
  } else if (p.startsWith('/terminal') || p.startsWith('/metrics') || p.startsWith('/analytics')) {
    activeTab.value = 'operations';
  } else if (p.startsWith('/sources') || p.startsWith('/source/') || p.startsWith('/security/cloud') || p.startsWith('/security/integration')) {
    activeTab.value = 'sources';
  } else if (p.startsWith('/vulnerabilities') || p.startsWith('/security')) {
    activeTab.value = 'security';
  } else if (
    p.startsWith('/teams') ||
    p.startsWith('/team') ||
    p.startsWith('/settings') ||
    p.startsWith('/notifications') ||
    p.startsWith('/profile') ||
    p.startsWith('/transfers') ||
    p.startsWith('/shared-variables') ||
    p.startsWith('/tags') ||
    p.startsWith('/subscription') ||
    p.startsWith('/admin') ||
    p.startsWith('/onboarding')
  ) {
    activeTab.value = 'settings';
  } else {
    activeTab.value = 'overview';
  }
}

function handleServerCreated(uuid: string) {
  activeServerUuid.value = uuid;
  activeResourceUuid.value = uuid;
  serverListRef.value?.refresh();
}

function handleServerSelected(uuid: string) {
  activeServerUuid.value = uuid;
  activeResourceUuid.value = uuid;
}

function handleDatabaseSelected(uuid: string, dbType: string) {
  activeDatabaseUuid.value = uuid;
  activeDatabaseType.value = dbType;
  activeResourceUuid.value = uuid;
}

onMounted(() => {
  resolveTabFromPath();
  subscribeRealtimeToasts();
});

onBeforeUnmount(() => {
  if (eventStream) {
    eventStream.close();
    eventStream = null;
  }
});
</script>

<template>
  <div style="display:grid;gap:14px;">
    <TeamAccess />

    <div style="display:flex;justify-content:space-between;align-items:center;gap:8px;flex-wrap:wrap;">
      <p v-if="realtimeToast" role="status" data-testid="realtime-toast" style="margin:0;font-size:13px;">
        {{ realtimeToast }}
      </p>
      <span v-else style="font-size:12px;color:var(--ui-fg-faint);">Realtime channel active</span>
      <button type="button" class="coolify-btn" data-testid="help-toggle-btn" @click="showHelp = !showHelp">
        {{ showHelp ? 'Close Help' : 'Help & Feedback' }}
      </button>
    </div>

    <div v-if="showHelp" class="coolify-card" data-testid="help-drawer">
      <form class="coolify-well" style="display:flex;gap:8px;flex-wrap:wrap;align-items:center;" @submit.prevent="submitHelpFeedback">
        <input v-model="helpSubject" class="coolify-input" placeholder="Feedback subject" required />
        <input v-model="helpDescription" class="coolify-input" style="flex:1;min-width:220px;" placeholder="Describe issue or feedback" required />
        <button type="submit" class="coolify-btn coolify-btn-primary">Send Feedback</button>
        <span v-if="helpStatus" role="status">{{ helpStatus }}</span>
      </form>
    </div>

    <div role="tablist" aria-label="Workspace sections" style="display:flex;gap:6px;flex-wrap:wrap;">
      <button
        v-for="tab in ([
          ['overview', 'Overview'],
          ['servers', 'Servers'],
          ['projects', 'Projects & Deploy'],
          ['operations', 'Logs & Terminal'],
          ['databases', 'Databases & Backups'],
          ['sources', 'Sources & Cloud'],
          ['security', 'Security & Scans'],
          ['settings', 'Settings & Transfer'],
        ] as Array<[TabKey, string]>)"
        :key="tab[0]"
        type="button"
        role="tab"
        :aria-selected="activeTab === tab[0]"
        class="coolify-btn"
        :class="{ 'coolify-btn-primary': activeTab === tab[0] }"
        :data-testid="`tab-${tab[0]}`"
        @click="activeTab = tab[0]"
      >
        {{ tab[1] }}
      </button>
    </div>

    <template v-if="activeTab === 'overview'">
      <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">
        <ServerList ref="serverListRef" @select="handleServerSelected" />
        <ServerSummary :server-uuid="activeServerUuid" />
      </div>
      <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">
        <ServerForm @created="handleServerCreated" />
        <ProjectExplorer @select="resourcesManagerRef?.refresh()" />
      </div>
      <CanvasGraph />
    </template>

    <template v-else-if="activeTab === 'servers'">
      <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">
        <ServerList ref="serverListRef" @select="handleServerSelected" />
        <ServerForm @created="handleServerCreated" />
      </div>
      <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">
        <ServerSummary :server-uuid="activeServerUuid" />
        <ServerSettings :server-uuid="activeServerUuid" />
      </div>
      <ServerOpsPanel :server-uuid="activeServerUuid" />
      <ProxyConfig :server-uuid="activeServerUuid" />
    </template>

    <template v-else-if="activeTab === 'projects'">
      <ProjectExplorer @select="resourcesManagerRef?.refresh()" />
      <ResourcesManager
        ref="resourcesManagerRef"
        @select-app="(u) => { activeApplicationUuid = u; activeResourceUuid = u; }"
        @select-service="(u) => { activeServiceUuid = u; activeResourceUuid = u; }"
      />
      <DeployControl
        v-if="activeApplicationUuid"
        :application-uuid="activeApplicationUuid"
        @deployed="deployHistoryRef?.refresh()"
      />
      <DeploymentHistory v-if="activeApplicationUuid" ref="deployHistoryRef" :application-uuid="activeApplicationUuid" />
      <ServiceEditor v-if="activeServiceUuid" :service-uuid="activeServiceUuid" />
      <ScheduleList
        v-if="activeApplicationUuid || activeServiceUuid"
        :application-uuid="activeApplicationUuid"
        :service-uuid="activeServiceUuid"
      />
    </template>

    <template v-else-if="activeTab === 'operations'">
      <div class="coolify-card">
        <div class="coolify-well" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;">
          <label>Log Resource UUID: <input v-model="activeResourceUuid" class="coolify-input" data-testid="log-resource-input" /></label>
          <label>Terminal Server UUID: <input v-model="activeServerUuid" class="coolify-input" /></label>
        </div>
      </div>
      <LogViewer :resource-uuid="activeResourceUuid" />
      <WebTerminal :server-uuid="activeServerUuid" />
    </template>

    <template v-else-if="activeTab === 'databases'">
      <DatabasePanel
        :database-uuid="activeDatabaseUuid"
        :db-type="activeDatabaseType"
        @select="handleDatabaseSelected"
      />
      <BackupManager
        :database-uuid="activeDatabaseUuid"
        @updated="scheduleListRef?.refresh()"
      />
      <ScheduleList ref="scheduleListRef" :database-uuid="activeDatabaseUuid" />
    </template>

    <template v-else-if="activeTab === 'sources'">
      <SourcesAndCloudPanel />
    </template>

    <template v-else-if="activeTab === 'security'">
      <SecurityAndVulnPanel :server-uuid="activeServerUuid" />
    </template>

    <template v-else-if="activeTab === 'settings'">
      <GovernanceAndAuthPanel />
      <ApiTokenManager />
      <NotificationSettings />
      <ServerTransferPanel :server-uuid="activeServerUuid" />
      <ProxyConfig :server-uuid="activeServerUuid" />
    </template>
  </div>
</template>
