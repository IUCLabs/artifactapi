// repoClass distinguishes the three ways a repository is consumed. remotes are
// caching proxies, locals are real registries you also publish to, virtuals are
// merged read-only indexes.
export type RepoClass = 'remote' | 'local' | 'virtual';

export interface Snippet {
  title: string;
  language: string;
  code: string;
  note?: string;
}

export interface SnippetContext {
  packageType: string;
  name: string;
  url: string;
  host: string;
  proxy: string;
  isLocal: boolean;
}

export type BadgeColor = 'blue' | 'green' | 'yellow' | 'red' | 'default';

export interface PackageTypeUI {
  color?: BadgeColor;
  snippets: (ctx: SnippetContext) => Snippet[];
}
