import { createRequire } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";

export type PeerLoader = (specifier: string) => unknown;

function resolveAnchorRequire(): NodeRequire {
  return createRequire(import.meta.url);
}

function isModuleNotFound(error: unknown): boolean {
  const code = (error as NodeJS.ErrnoException | undefined)?.code;
  return code === "MODULE_NOT_FOUND" || code === "ERR_MODULE_NOT_FOUND";
}

/**
 * Synchronously load an optional peer dependency on demand so importing `@lazy-observe/node`
 * never requires optional integrations (`pino`, `winston-transport`) to be installed.
 */
export function loadOptionalPeer<T>(
  specifier: string,
  feature: string,
  loader?: PeerLoader,
): T {
  const load = loader ?? ((id: string) => {
    try {
      return resolveAnchorRequire()(id);
    } catch (primaryError) {
      if (!isModuleNotFound(primaryError)) throw primaryError;
      return createRequire(pathToFileURL(path.join(process.cwd(), "package.json")).href)(id);
    }
  });
  try {
    const loaded = load(specifier) as { default?: T } & T;
    if (loaded && typeof loaded === "object" && "default" in loaded && loaded.default !== undefined) {
      return loaded.default;
    }
    return loaded;
  } catch (error) {
    if (isModuleNotFound(error)) {
      throw new Error(
        `@lazy-observe/node: ${feature} requires optional peer dependency "${specifier}". Install it in your application (for example: pnpm add ${specifier}).`,
      );
    }
    throw error;
  }
}
