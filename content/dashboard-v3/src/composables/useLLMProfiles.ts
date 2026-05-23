/**
 * useLLMProfiles — TanStack-backed query for the chat backend's
 * profile catalog (GET /api/v2/chat/profiles).
 *
 * Catalog is small (<5KB) and changes only on operator edit — cache
 * indefinitely with a manual refetch button (rare).
 */

import { useQuery } from '@tanstack/vue-query';
import { fetchProfiles } from '@/repo/chat-repo';
import type { ChatCatalog } from '@/types/chat';

export function useLLMProfiles() {
  return useQuery<ChatCatalog>({
    queryKey: ['llm', 'profiles'],
    queryFn: fetchProfiles,
    staleTime: Infinity,
    retry: 1,
  });
}
