export declare const parseUrl: (url: unknown, defaultOrigin?: string) => URL | null;
export declare const isPathRelativeUrl: (url: unknown) => boolean;
export declare const transformUrl: (url: unknown, allowedPrefixes: string[], defaultOrigin?: string) => string | null;
