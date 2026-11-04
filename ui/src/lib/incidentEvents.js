// Incident updates have no replay cursor. An opened connection and a
// resync invalidate local incident state just like a live update does.
export function forwardIncidentEvents(source, target, refreshOverview) {
  const refresh = () => {
    target.dispatchEvent(new Event('watcherbridge:incident'));
    refreshOverview();
  };
  const events = ['open', 'incident', 'resync'];
  for (const event of events) {
    source.addEventListener(event, refresh);
  }
  return () => {
    for (const event of events) {
      source.removeEventListener(event, refresh);
    }
  };
}
