# Production dependency licenses

| Package | License |
|---|---|
| `rrweb`, `@rrweb/types`, `@rrweb/utils`, `rrweb-snapshot`, `rrdom`, `mitt`, `base64-arraybuffer`, `@xstate/fsm`, `@types/css-font-loading-module`, `postcss`, `nanoid` | MIT |
| `web-vitals` | Apache-2.0 |
| `source-map-js` | BSD-3-Clause |
| `picocolors` | ISC (the simplified BSD-family license) |
| `react`, `react-dom`, `scheduler` (React adapter peer/demo) | MIT |

Build and test tooling is not shipped in the runtime packages. The authoritative installed-tree audit is `pnpm --filter @lazy-observe/browser list --prod --depth Infinity` plus the installed packages' license metadata. CI should reject copyleft or licenses outside MIT, Apache-2.0 and BSD-style families.
