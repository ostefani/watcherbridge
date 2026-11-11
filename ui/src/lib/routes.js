// Hash route table. Single source of truth for route names, patterns
// and document titles — replaces App's inline parseRoute and title
// map. Matching behavior is unchanged: first match wins, briefing and
// incident IDs are decoded, anything unrecognized is home.

const routes = [
  { name: 'briefing', pattern: /^#\/briefing\/(.+)$/ },
  { name: 'incident', pattern: /^#\/incidents\/(.+)$/ },
  { name: 'device', pattern: /^#\/devices\/(.+)$/ },
  { name: 'devices', pattern: /^#\/devices\/?$/ },
  { name: 'health', pattern: /^#\/health$/ }
];

const titles = {
  home: 'Review overview',
  devices: 'Reported hosts',
  device: 'Reported host detail',
  briefing: 'Briefing',
  incident: 'Incident',
  health: 'Health'
};

/**
 * @param {string|null|undefined} hash
 * @returns {{name: string, id?: string}}
 */
export function parseRoute(hash) {
  const h = hash || '#/';
  for (const route of routes) {
    const m = h.match(route.pattern);
    if (m) {
      return m[1] === undefined ? { name: route.name } : { name: route.name, id: decodeURIComponent(m[1]) };
    }
  }
  return { name: 'home' };
}

/** @param {string} name */
export function routeTitle(name) {
  return titles[name] ?? titles.home;
}
