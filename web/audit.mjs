// npm audit has no flag to ignore one advisory.
const ignored = new Map([
  ['GHSA-fc5q-39xc-vjpf', 'picomatch ReDoS through vite, a build-time dependency; no patched release'],
]);
const levels = ['info', 'low', 'moderate', 'high', 'critical'];
const minLevel = levels.indexOf('moderate');

let input = '';
for await (const chunk of process.stdin) input += chunk;
const report = JSON.parse(input);
if (report.error || !report.vulnerabilities) {
  console.error('npm audit failed:', JSON.stringify(report.error ?? report));
  process.exit(1);
}

const failing = new Map();
const skipped = new Set();
for (const vuln of Object.values(report.vulnerabilities)) {
  for (const via of vuln.via) {
    if (typeof via !== 'object') continue;
    const id = via.url.split('/').pop();
    if (levels.indexOf(via.severity) < minLevel) continue;
    if (ignored.has(id)) skipped.add(id);
    else failing.set(id, `${via.severity} ${via.name}: ${via.title} (${via.url})`);
  }
}

for (const id of skipped) console.log(`ignored ${id}: ${ignored.get(id)}`);
for (const id of ignored.keys()) {
  if (!skipped.has(id)) console.log(`${id} is no longer reported; remove it from web/audit.mjs`);
}
if (failing.size > 0) {
  for (const line of failing.values()) console.error(line);
  process.exit(1);
}
console.log('no unignored advisories at moderate or above');
