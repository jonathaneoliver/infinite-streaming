/**
 * useLLMBudget — polling query for the chat backend's global budget
 * meter (GET /api/v2/chat/budget).
 *
 * Polled every 30s when a chat panel is open. Refetch is also
 * triggered explicitly after a chat completes via queryClient.
 */

import { useQuery } from '@tanstack/vue-query';
import { fetchBudget } from '@/repo/chat-repo';
import type { BudgetStatus } from '@/types/chat';

export function useLLMBudget(enabled = true) {
  return useQuery<BudgetStatus>({
    queryKey: ['llm', 'budget'],
    queryFn: fetchBudget,
    refetchInterval: enabled ? 30_000 : false,
    staleTime: 10_000,
    retry: 1,
  });
}
