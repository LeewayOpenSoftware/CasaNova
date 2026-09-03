/**
 * Wrap a polling callback so it does nothing while the tab is hidden.
 *
 * The reason this exists is stated best by stores/system.ts, which has done
 * it since Wave 32: "A backgrounded tab cannot show the result, and on a NAS
 * every tick is real work on the box." A Settings tab left open in the
 * background was re-scanning disks, sync status and utilization every 2-3
 * seconds indefinitely, competing with the user's own apps for the same CPU
 * and spindles.
 *
 * Deliberately a skip and not a stop: the timer keeps its cadence, so the
 * first tick after the tab comes back refreshes normally without any
 * visibilitychange listener to register, own and eventually leak in ten
 * different components. The cost is that returning to a tab can show data up
 * to one interval old (at most 60s, for the two dashboard widgets) before it
 * refreshes on its own.
 *
 * Stores that need an immediate catch-up on return (system, virt) keep their
 * own visibilitychange handler; this helper is for the plain component-scoped
 * pollers.
 */
export function skipWhenHidden(fn: () => void): () => void {
  return () => {
    if (typeof document !== 'undefined' && document.hidden) return
    fn()
  }
}
