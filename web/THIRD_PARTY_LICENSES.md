# Web third-party licenses

The production web application includes the following Prometheus-maintained
package for the PromQL query tree and Explain view:

- `@prometheus-io/lezer-promql` 0.313.1

Copyright 2021 The Prometheus Authors.

These packages are licensed under the Apache License, Version 2.0. Their full
license text is distributed in each npm package as `LICENSE` and is available
from <https://www.apache.org/licenses/LICENSE-2.0>.

The MetricsQL input uses the following CodeMirror 6 packages under the MIT
license:

- `@codemirror/autocomplete`
- `@codemirror/commands`
- `@codemirror/language`
- `@codemirror/state`
- `@codemirror/view`
- `@lezer/highlight`

Their license texts are distributed in the corresponding npm packages.

The metrics time-series charts and logs histograms use `uplot` 1.6.32,
Copyright (c) 2019 Leon Sorokin, under the MIT license. The full license text
is distributed in the npm package as `LICENSE`.

Date parsing, formatting, localization, and timezone handling use `dayjs`
1.11.21, Copyright (c) 2018-present, iamkun, under the MIT license. The
application statically includes only the English and Simplified Chinese locale
modules. The full license text is distributed in the npm package as `LICENSE`.

The query-workspace layout and application state in lazy-observe were rewritten
for this project after reviewing Prometheus' Expression Browser behavior; no
Prometheus QueryPage, QueryPanel, graph, table, or metrics-explorer source file
was copied into this repository. The MetricsQL editor integration and its
project-scoped completion behavior are implemented in this repository rather
than using Prometheus' CodeMirror editor or linter.

The logs explorer was independently implemented for lazy-observe after
reviewing the VictoriaLogs UI Query page from the Apache-2.0-licensed
VictoriaLogs repository. No VictoriaLogs UI source file or runtime dependency
is copied or bundled into this application; the implementation uses the
project's existing React, CodeMirror, and uPlot stack and communicates only
through lazy-observe's project-scoped gateway routes.
