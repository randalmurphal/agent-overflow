// Byte-faithful content from the 2026-08-29 22:58 incident (thread bc48c862, turn 4).
export const INCIDENT_TEXT_DELTAS: readonly string[] = [
  "Two investigation agents running for item 4:\n\n1. **Repo survey** — full vendor inventory, which ex",
  "ports the app actually uses vs dead upstream features, mapping all 25 divergences to their first-party fate, what the marked patch really chang",
  "es, target layout under `src/`, and which vendor deps (tailwind-merge, floating-ui, clsx…) can be dropped.\n2. **Upstream research** — mar",
  "ked 16→18 breaking changes (we're two majors behind; you downgraded to 16.4.2 to dodge the dual-copy problem), wh",
  "ether newer marked fixes the substring-retention issue our patch works around, katex 0.16→0.18 and mermaid/shiki gaps, and whether upstream streamdown merged anything wor",
  "th stealing before we cut ties.\n\nAlready confirmed from a quick look: the vendor tree carries its own `node_modules` with duplicate marked/katex/mermaid/shiki copies, and there are two",
  " more local patches in play (`svelte@5.56.8.patch`, `@lucide__svelte@1.28.0.patch`) — the svelte one is being checked for entanglement with the ren",
  "derer. Report when both land."
];
export const INCIDENT_TEXT_FULL = INCIDENT_TEXT_DELTAS.join("");
export const INCIDENT_THINK_DELTAS: readonly string[] = [
  "I",
  "'ve launched two agents to dig deeper. In the meantime, I'm noting some quick facts: marked is at 18.0.11 versus our 16.4.2 (two major versions behind), katex is",
  " two minors behind (0.16→0.18), mermaid is slightly behind, and the vendor directory has its own node_modules with duplicate copies. I'll report back once the agents finish."
];
export const INCIDENT_THINK_FULL = INCIDENT_THINK_DELTAS.join("");
