import type { MermaidConfig } from 'mermaid';

// The app's mermaid rendering defaults, chosen against real renders of
// the app's diagrams under mermaid 12.
//
// Mermaid 12 lays flowchart, state and class diagrams out with ELK and
// draws them in its "neo" look by default. ELK routes edges at right
// angles but places parallel edge labels a few pixels apart, so two
// labels leaving one node read as one phrase; dagre keeps them apart.
// The neo look stays. Its drop shadow, whose light colour is hardcoded
// in mermaid's stylesheet, is removed in app.css.
//
// `flowchart.curve` also shapes state and class edges. A stepped curve
// gives flowcharts the right-angle lines ELK would have drawn, but
// rotates class diagram markers to follow the final segment (inheritance
// triangles sideways, diamonds over the cardinality), so only flowcharts
// take it.

export const FLOWCHART_CURVE = 'stepAfter';
export const DEFAULT_CURVE = 'basis';

/** Mermaid's detector names for flowcharts (`flowchart` and `graph`
 * sources), whatever their layout suffix. */
export function isFlowchartType(diagramType: string): boolean {
  return diagramType === 'flowchart' || diagramType.startsWith('flowchart-');
}

/** The config a diagram of `diagramType` renders with; the host's theme
 * config is merged over it. `diagramType` is mermaid's `detectType`
 * result, or '' when detection failed (the render then fails the same
 * way with any config). */
export function mermaidRenderConfig(diagramType: string): MermaidConfig {
  return {
    theme: 'base',
    startOnLoad: false,
    securityLevel: 'strict',
    fontFamily: 'monospace',
    suppressErrorRendering: true,
    layout: 'dagre',
    look: 'neo',
    flowchart: {
      useMaxWidth: true,
      htmlLabels: true,
      curve: isFlowchartType(diagramType) ? FLOWCHART_CURVE : DEFAULT_CURVE,
    },
  };
}
