<script setup lang="ts">
import { ref } from 'vue';
import { apiRequest } from '../lib/api';
import { pushToast } from '../lib/toast';

defineProps<{
    hasProjects: boolean;
    hasReadyServer: boolean;
}>();

const emit = defineEmits<{ completed: [] }>();
const step = ref(1);
const busy = ref(false);

async function completeOnboarding() {
    if (busy.value) return;
    busy.value = true;
    try {
        await apiRequest('/app/boarding/complete', { method: 'POST', body: '{}' });
        pushToast('Onboarding completed.', 'success');
        emit('completed');
    } catch (err) {
        pushToast((err as Error).message, 'error');
    } finally {
        busy.value = false;
    }
}
</script>

<template>
    <section
        class="onboarding-card"
        aria-label="Onboarding"
        data-testid="onboarding-wizard"
    >
        <div class="onboarding-header">
            <div>
                <h2>Welcome to Goolify</h2>
                <p class="page-muted">
                    Complete these quick steps to get your first server and project ready.
                </p>
            </div>
            <button
                type="button"
                class="coolify-btn"
                :disabled="busy"
                @click="completeOnboarding"
            >
                Skip onboarding
            </button>
        </div>
        <div class="onboarding-steps">
            <div
                class="onboarding-step"
                :class="{ active: step === 1, done: hasReadyServer }"
            >
                <strong>1. Server Ready</strong>
                <p>
                    {{
                        hasReadyServer
                            ? 'A validated server is ready to deploy resources.'
                            : 'Validate localhost or connect a remote SSH server.'
                    }}
                </p>
                <div class="page-actions">
                    <a
                        href="/servers"
                        class="coolify-btn"
                        >Open Servers</a
                    >
                    <button
                        v-if="step === 1"
                        type="button"
                        class="coolify-btn coolify-btn-primary"
                        @click="step = 2"
                    >
                        Next
                    </button>
                </div>
            </div>
            <div
                class="onboarding-step"
                :class="{ active: step === 2, done: hasProjects }"
            >
                <strong>2. First Project</strong>
                <p>
                    {{
                        hasProjects
                            ? 'You already have a project configured.'
                            : 'Create a project to organize your applications, services, and databases.'
                    }}
                </p>
                <div class="page-actions">
                    <a
                        href="/projects"
                        class="coolify-btn"
                        >Open Projects</a
                    >
                    <button
                        v-if="step === 2"
                        type="button"
                        class="coolify-btn coolify-btn-primary"
                        @click="step = 3"
                    >
                        Next
                    </button>
                </div>
            </div>
            <div
                class="onboarding-step"
                :class="{ active: step === 3 }"
            >
                <strong>3. Ready to Deploy</strong>
                <p>You can now deploy Git repositories, Dockerfiles, Compose stacks, and databases.</p>
                <div class="page-actions">
                    <button
                        type="button"
                        class="coolify-btn coolify-btn-primary"
                        :disabled="busy"
                        @click="completeOnboarding"
                    >
                        Finish Onboarding
                    </button>
                </div>
            </div>
        </div>
    </section>
</template>
