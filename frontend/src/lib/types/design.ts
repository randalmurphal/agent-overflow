export interface DesignArtifact {
  id: string;
  threadId: string;
  title: string;
  description: string;
  kind: 'render' | 'option';
  htmlPath: string;
  createdAt: number;
}

export interface DesignOption {
  id: string;
  title: string;
  description: string;
  artifactId: string;
}

export interface DesignOptionsRequest {
  requestId: string;
  threadId: string;
  prompt: string;
  options: DesignOption[];
}
