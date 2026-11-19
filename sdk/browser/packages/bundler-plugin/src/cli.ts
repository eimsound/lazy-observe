#!/usr/bin/env node
import { parseArgs } from "node:util";
import { uploadSourceMaps } from "./upload";

const usage = `Usage: lazy-observe-sourcemaps upload --dir <directory> [--dsn <dsn>] [--release <release>] [--strip]

Uploads every .map file under <directory> to lazy-observe.

Options:
  --dir <directory>    Directory scanned recursively for .map files (required)
  --dsn <dsn>          <origin>/p/<project>@<token>; defaults to $LO_DSN.
                       Without a DSN the command warns and exits 0 without uploading.
  --release <release>  Release key; defaults to $LO_RELEASE, then the seven-character Git SHA
  --strip              Delete the local .map files after a successful upload
  -h, --help           Show this help
`;

async function main(argv: string[], env: NodeJS.ProcessEnv): Promise<number> {
  let parsed: ReturnType<typeof parse>;
  try { parsed = parse(argv); }
  catch (error) {
    process.stderr.write(`lazy-observe-sourcemaps: ${(error as Error).message}\n\n${usage}`);
    return 2;
  }
  const { values, positionals } = parsed;
  if (values.help || positionals[0] === "help") {
    process.stdout.write(usage);
    return 0;
  }
  if (positionals.length !== 1 || positionals[0] !== "upload") {
    process.stderr.write(`lazy-observe-sourcemaps: expected the "upload" command\n\n${usage}`);
    return 2;
  }
  if (!values.dir) {
    process.stderr.write(`lazy-observe-sourcemaps: --dir is required\n\n${usage}`);
    return 2;
  }
  try {
    const result = await uploadSourceMaps({
      dsn: values.dsn ?? env.LO_DSN,
      release: values.release ?? env.LO_RELEASE,
      dir: values.dir,
      stripSourceMaps: values.strip,
      warn: (message) => process.stderr.write(`${message}\n`)
    });
    if (result.uploaded) process.stdout.write(`lazy-observe: uploaded ${result.uploaded} source map(s) for release ${result.release}\n`);
    return 0;
  } catch (error) {
    process.stderr.write(`${(error as Error).message}\n`);
    return 1;
  }
}

function parse(argv: string[]) {
  return parseArgs({
    args: argv,
    allowPositionals: true,
    options: {
      dir: { type: "string" },
      dsn: { type: "string" },
      release: { type: "string" },
      strip: { type: "boolean" },
      help: { type: "boolean", short: "h" }
    }
  });
}

process.exitCode = await main(process.argv.slice(2), process.env);
