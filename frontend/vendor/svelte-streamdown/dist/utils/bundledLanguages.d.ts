export type LanguageInfo = {
    id: string;
    aliases?: string[];
    import: () => Promise<any>;
};
export declare const bundledLanguagesInfo: LanguageInfo[];
export declare function createLanguageSet(languages: LanguageInfo[]): Set<string>;
export declare const supportedLanguages: Set<string>;
