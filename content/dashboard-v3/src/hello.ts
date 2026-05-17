import { createApp, h, ref, onMounted } from 'vue';

const App = {
  setup() {
    const status = ref<string>('loading…');
    const playerCount = ref<number | null>(null);
    onMounted(async () => {
      try {
        const r = await fetch('/api/v2/info');
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const info = await r.json();
        const players = await fetch('/api/v2/players').then((p) => p.json());
        playerCount.value = (players.items || []).length;
        status.value = `OK — proxy ${info.version ?? 'unknown'}, ${playerCount.value} players`;
      } catch (err) {
        status.value = `error: ${(err as Error).message}`;
      }
    });
    const codeStyle =
      'background:#1f2937;color:#e5e7eb;padding:2px 6px;border-radius:4px;font-family:ui-monospace,monospace;font-size:90%';
    return () =>
      h(
        'div',
        {
          style:
            'font-family:system-ui,-apple-system,sans-serif;padding:32px;max-width:720px;margin:0 auto;color:#111;background:#fff;min-height:100vh;line-height:1.5;',
        },
        [
          h('h1', { style: 'margin:0 0 12px 0;font-size:28px;color:#111' }, 'InfiniteStream Dashboard v3'),
          h('p', { style: 'color:#374151' }, 'Vue 3 + TanStack Query stack is live.'),
          h('p', { style: 'color:#374151' }, status.value),
          h('p', { style: 'color:#6b7280;font-size:13px' }, [
            'API: ',
            h('code', { style: codeStyle }, '/api/v2/info'),
            ' + ',
            h('code', { style: codeStyle }, '/api/v2/players'),
          ]),
        ],
      );
  },
};

createApp(App).mount('#app');
