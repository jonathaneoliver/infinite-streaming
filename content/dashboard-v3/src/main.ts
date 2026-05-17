/**
 * Single entry point for every multi-page HTML in the v3 dashboard.
 * The page's <script src="...main.ts"> imports this and registers
 * exactly one of the page components into <div id="app">. Vite's
 * tree-shaking means each entry only ships the components its page
 * actually imports.
 */
import { createApp, defineAsyncComponent, h, type Component } from 'vue';
import { VueQueryPlugin, QueryClient } from '@tanstack/vue-query';

// Reasonable defaults for an internal dashboard. SSE keeps the cache
// fresh — the rare refetch happens on window focus or when stale.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      gcTime: 5 * 60_000,
      refetchOnWindowFocus: true,
      retry: 1,
    },
    mutations: {
      retry: 0,
    },
  },
});

export function mountPage(component: Component, selector = '#app') {
  const app = createApp(component);
  app.use(VueQueryPlugin, { queryClient });
  app.mount(selector);
  return app;
}

// Convenience for `import { mountAsync } from '@/main'` — async chunked
// imports so each page bundle stays small.
export function mountAsync(loader: () => Promise<Component | { default: Component }>, selector = '#app') {
  const Async = defineAsyncComponent(async () => {
    const mod = await loader();
    return (mod as any).default ?? (mod as any);
  });
  return mountPage({ render: () => h(Async) }, selector);
}

export { queryClient };
