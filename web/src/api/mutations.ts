import {
  useMutation,
  useQueryClient,
  type QueryKey,
  type UseMutationResult,
} from "@tanstack/react-query";

/**
 * A write, and the keys it makes stale.
 *
 * Nearly every mutation in this application is the same shape: call the API,
 * then invalidate the handful of queries that could now disagree with the
 * server. Which keys those are is the interesting part and stays at the call
 * site; the wiring around it does not.
 *
 * `after` is for the two writes that do something more than invalidate —
 * seeding the detail cache with the body the server just returned, so the
 * screen the operator lands on does not have to fetch what it was just handed.
 */
export function useInvalidating<TResult, TArgs = void>(
  call: (args: TArgs) => Promise<TResult>,
  invalidate: readonly QueryKey[],
  after?: (result: TResult, args: TArgs) => void,
): UseMutationResult<TResult, Error, TArgs> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: call,
    onSuccess: (result, args) => {
      after?.(result, args);
      for (const key of invalidate) {
        void client.invalidateQueries({ queryKey: key });
      }
    },
  });
}
