import { describe, expect, it } from 'vitest';
import {
  DEFAULT_CURVE,
  FLOWCHART_CURVE,
  isFlowchartType,
  mermaidRenderConfig,
} from './mermaidRenderConfig';

describe('mermaidRenderConfig', () => {
  it('steps flowchart edges and curves every other diagram', () => {
    for (const type of ['flowchart', 'flowchart-v2', 'flowchart-elk']) {
      expect(isFlowchartType(type)).toBe(true);
      expect(mermaidRenderConfig(type).flowchart?.curve).toBe(FLOWCHART_CURVE);
    }
    for (const type of ['classDiagram', 'stateDiagram', 'sequence', 'class', '']) {
      expect(isFlowchartType(type)).toBe(false);
      expect(mermaidRenderConfig(type).flowchart?.curve).toBe(DEFAULT_CURVE);
    }
  });

  it('pins dagre under the neo look so parallel edge labels stay apart', () => {
    const config = mermaidRenderConfig('flowchart-v2');
    expect(config.layout).toBe('dagre');
    expect(config.look).toBe('neo');
    expect(config.theme).toBe('base');
    expect(config.securityLevel).toBe('strict');
    expect(config.suppressErrorRendering).toBe(true);
  });
});
