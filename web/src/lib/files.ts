// File links in a model's answer. Through the hub every worker's files are under /files/<route>/… (docs/architecture.md,
// "Several Macs"): the worker gives file fields (files, images, a tool's files …) that prefix itself, but leaves the
// text the model wrote as it was — a /files/ path in an answer is the worker's own. The page adds the job's route
// to such links, so they open on the Mac that has the file.

/** The route of a job id through the hub ("m2" of "m2.153000-4"); undefined without one (mlxlab serve). */
export function routeOfJob(id: string | undefined): string | undefined {
  const m = /^([a-z0-9]{1,16})\.(.+)$/.exec(id ?? "");
  return m ? m[1] : undefined;
}

/** A /files/ url of a job's worker as the hub serves it (unchanged without a route, or when it has it already). */
export function routedFile(href: string, route: string | undefined): string {
  if (!route || !href.startsWith("/files/") || href.startsWith(`/files/${route}/`)) return href;
  return `/files/${route}/` + href.slice("/files/".length);
}
