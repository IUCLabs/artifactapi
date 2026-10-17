import { useState } from 'react';
import './UsageInstructions.css';
import { packageTypes } from '../packagetypes';
import type { RepoClass, Snippet } from '../packagetypes/types';

// The UI is served on the same origin as the API, so this is the address clients use.
function baseURL(): string {
  return window.location.origin;
}

// hostOnly is host[:port] for scheme-less docker/terraform addresses.
function hostOnly(): string {
  return window.location.host;
}

// remoteProxyBase is where a remote (or virtual) repo's proxied artifacts live.
function remoteProxyBase(cls: RepoClass, name: string): string {
  const seg = cls === 'virtual' ? 'virtual' : 'remote';
  return `${baseURL()}/api/v1/${seg}/${name}`;
}

export function buildSnippets(packageType: string, repoClass: RepoClass, name: string): Snippet[] {
  const url = baseURL();
  const host = hostOnly();
  const proxy = remoteProxyBase(repoClass, name);
  const isLocal = repoClass === 'local';

  const def = packageTypes[packageType] ?? packageTypes.generic;
  return def.snippets({ packageType, name, url, host, proxy, isLocal });
}

function CodeBox({ snippet }: { snippet: Snippet }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(snippet.code);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard API unavailable (e.g. non-secure context); silently ignore.
    }
  }

  return (
    <div className="usage-snippet">
      <div className="usage-snippet-title">{snippet.title}</div>
      <div className="usage-codebox">
        <button className="usage-copy-btn" onClick={copy} type="button">
          {copied ? 'copied' : 'copy'}
        </button>
        <pre className="mono">{snippet.code}</pre>
      </div>
      {snippet.note && <div className="usage-note">{snippet.note}</div>}
    </div>
  );
}

interface UsageInstructionsProps {
  packageType: string;
  repoClass: RepoClass;
  name: string;
  defaultOpen?: boolean;
}

export function UsageInstructions({ packageType, repoClass, name, defaultOpen = false }: UsageInstructionsProps) {
  const [open, setOpen] = useState(defaultOpen);
  const snippets = buildSnippets(packageType, repoClass, name);

  return (
    <div className="usage-panel">
      <button className="usage-toggle" onClick={() => setOpen(o => !o)} type="button" aria-expanded={open}>
        <span className={`usage-caret ${open ? 'open' : ''}`}>&#9656;</span>
        How do I use this?
      </button>
      {open && (
        <div className="usage-body">
          {snippets.map((s, i) => (
            <CodeBox key={i} snippet={s} />
          ))}
        </div>
      )}
    </div>
  );
}
