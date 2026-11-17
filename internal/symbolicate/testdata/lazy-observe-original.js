export function explode(value) {
  const adjusted = value + 1;
  throw new Error("boom " + adjusted);
}
explode(41);
