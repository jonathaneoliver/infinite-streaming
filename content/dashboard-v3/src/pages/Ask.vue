<script setup lang="ts">
/**
 * Ask.vue — full-width AI chat page (#497).
 *
 * Fleet-mode entry point. Goes deeper than the side-panel variant
 * mounted on the dashboards — full column width, persistent
 * conversation, plenty of room for citations + tool-call cards.
 */
import ShellLayout from '@/components/ShellLayout.vue';
import ChatPanel from '@/components/chat/ChatPanel.vue';
</script>

<template>
  <ShellLayout active-page="ask">
    <template #header>
      <div class="page-title-bar">Ask</div>
    </template>
    <main class="ask-main">
      <div class="ask-intro">
        <h1>Ask the analytics</h1>
        <p>
          Fleet-mode AI chat over archived sessions. Ask about specific plays, look
          for aberrant sessions, compare runs — the bot has tools to query
          ClickHouse, read findings, and produce clickable citations.
        </p>
      </div>
      <div class="ask-chat-frame">
        <ChatPanel
          :scope="{ kind: 'fleet' }"
          scope-key="ask:fleet"
          variant="fluid"
        />
      </div>
    </main>
  </ShellLayout>
</template>

<style scoped>
.page-title-bar { font-size: 16px; font-weight: 600; }
.ask-main {
  max-width: 920px;
  margin: 0 auto;
  padding: 24px 24px 0;
  display: flex;
  flex-direction: column;
  gap: 16px;
  height: calc(100vh - var(--header-height, 64px));
  box-sizing: border-box;
}
.ask-intro h1 {
  font-size: 22px;
  font-weight: 600;
  margin: 0 0 4px;
}
.ask-intro p {
  color: var(--text-secondary);
  font-size: 13px;
  max-width: 600px;
  margin: 0;
}
.ask-chat-frame {
  flex: 1;
  min-height: 400px;
  padding-bottom: 24px;
  overflow: hidden;
  display: flex;
}
</style>
