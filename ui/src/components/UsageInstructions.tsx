import { useState } from 'react';
import './UsageInstructions.css';

// repoClass distinguishes the three ways a repository is consumed. remotes are
// caching proxies, locals are real registries you also publish to, virtuals are
// merged read-only indexes.
type RepoClass = 'remote' | 'local' | 'virtual';

interface Snippet {
  title: string;
  language: string;
  code: string;
  note?: string;
}

// baseURL resolves the externally reachable origin of this artifactapi instance.
// The UI is served on the same origin as the API (client BASE is ''), so
// window.location.origin is the address a host would actually curl/pull against
// — no hardcoded hostname, works in prod and in `npm run dev` behind a proxy.
function baseURL(): string {
  if (typeof window !== 'undefined' && window.location?.origin) {
    return window.location.origin.replace(/\/$/, '');
  }
  return 'https://artifactapi.example.com';
}

// hostOnly is the bare host[:port] with no scheme, for docker/terraform source
// addresses which are scheme-less.
function hostOnly(): string {
  try {
    return new URL(baseURL()).host;
  } catch {
    return 'artifactapi.example.com';
  }
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

  switch (packageType) {
    case 'rpm':
      return [
        {
          title: isLocal
            ? 'Add the yum repo (real yum repo, repodata auto-regenerated)'
            : 'Add the yum repo (caching proxy)',
          language: 'bash',
          code: `sudo tee /etc/yum.repos.d/${name}.repo >/dev/null <<'EOF'
[${name}]
name=${name} (artifactapi)
baseurl=${isLocal ? `${url}/api/v2/remotes/${name}/files/` : `${proxy}/`}
enabled=1
gpgcheck=0
repo_gpgcheck=0
EOF

sudo dnf install <package>`,
          note: isLocal
            ? 'gpgcheck=0: artifactapi serves the repo unsigned. If you sign your RPMs, import your key and set gpgcheck=1.'
            : 'gpgcheck=0 trusts upstream over the proxy. To verify package signatures, import the upstream GPG key and set gpgcheck=1.',
        },
        ...(isLocal
          ? [
              {
                title: 'Publish an RPM (repodata regenerates automatically)',
                language: 'bash',
                code: `curl -fsSL --upload-file ./my-package-1.0-1.el9.x86_64.rpm \\
  ${url}/api/v2/remotes/${name}/files/my-package-1.0-1.el9.x86_64.rpm`,
              },
            ]
          : []),
      ];

    case 'pypi':
      return [
        {
          title: 'Install a package (one-off)',
          language: 'bash',
          code: `pip install --index-url ${proxy}/simple/ <package>`,
        },
        {
          title: 'Configure pip persistently',
          language: 'bash',
          code: `mkdir -p ~/.config/pip
cat > ~/.config/pip/pip.conf <<'EOF'
[global]
index-url = ${proxy}/simple/
EOF

pip install <package>`,
        },
      ];

    case 'npm':
      return [
        {
          title: 'Point npm at this registry',
          language: 'bash',
          code: `npm config set registry ${proxy}/
npm install <package>`,
        },
        {
          title: 'Per-project (.npmrc)',
          language: 'bash',
          code: `echo 'registry=${proxy}/' >> .npmrc
npm install`,
        },
      ];

    case 'docker':
      return [
        {
          title: 'Pull an image',
          language: 'bash',
          code: `docker pull ${host}/${name}/<image>:<tag>`,
          note: 'The first path segment after the host is the artifactapi repo name; the rest is the image name.',
        },
        ...(isLocal
          ? [
              {
                title: 'Push an image (this is a real Registry V2)',
                language: 'bash',
                code: `docker tag myapp:latest ${host}/${name}/myapp:latest
docker push        ${host}/${name}/myapp:latest`,
                note: 'Works with docker, podman, skopeo and buildah. If the registry requires auth, run `docker login ' + host + '` first.',
              },
            ]
          : []),
      ];

    case 'terraform':
      return [
        {
          title: 'Use as a provider source (bare address, no mirror config)',
          language: 'hcl',
          code: `terraform {
  required_providers {
    ${name} = {
      source  = "${host}/${name}/<type>"
      version = ">= 0.1.0"
    }
  }
}`,
          note: 'The namespace segment is this repo name; <type> is the provider type. artifactapi signs SHA256SUMS server-side with its GPG key, so `terraform init` installs with no .terraformrc.',
        },
        ...(isLocal
          ? [
              {
                title: 'Publish a provider build',
                language: 'bash',
                code: `curl -fsSL --upload-file terraform-provider-<type>_0.1.0_linux_amd64.zip \\
  ${url}/api/v2/remotes/${name}/files/${name}/<type>/terraform-provider-<type>_0.1.0_linux_amd64.zip`,
              },
            ]
          : []),
      ];

    case 'helm':
      return [
        {
          title: 'Add the Helm repo',
          language: 'bash',
          code: `helm repo add ${name} ${proxy}/
helm repo update
helm install <release> ${name}/<chart>`,
        },
      ];

    case 'alpine':
      return isLocal
        ? [
            {
              title: 'Add the apk repo (real apk repo, APKINDEX auto-generated)',
              language: 'bash',
              code: `echo '${url}/api/v1/local/${name}' | sudo tee -a /etc/apk/repositories
sudo apk update --allow-untrusted
sudo apk add --allow-untrusted <package>`,
              note: `Served unsigned (parity with the rpm repo's gpgcheck=0) — use --allow-untrusted, or install a signing key. apk fetches <arch>/APKINDEX.tar.gz under this base.`,
            },
            {
              title: 'Publish a .apk (index regenerates automatically)',
              language: 'bash',
              code: `curl -fsSL --upload-file ./mypkg-1.0-r0.apk \\
  ${url}/api/v2/remotes/${name}/files/x86_64/mypkg-1.0-r0.apk`,
              note: 'Upload each package at <arch>/<name>-<version>.apk — apk reconstructs that exact path from the index (APKINDEX carries no filename), so a mismatched path will 404 on install.',
            },
          ]
        : [
            {
              title: 'Add the APK repository',
              language: 'bash',
              code: `echo '${proxy}/' | sudo tee -a /etc/apk/repositories
sudo apk update
sudo apk add <package>`,
              note: 'If the index is unsigned over the proxy, add --allow-untrusted or install the signing key into /etc/apk/keys.',
            },
          ];

    case 'github_alpine':
      return [
        {
          title: 'Add the apk repo (metadata-only, from GitHub releases)',
          language: 'bash',
          code: `echo '${proxy}' | sudo tee -a /etc/apk/repositories
sudo apk update --allow-untrusted
sudo apk add --allow-untrusted <package>`,
          note: "The per-arch APKINDEX is synthesized from the configured GitHub repo's release .apk assets; package downloads are redirected to the backing releases remote. Served unsigned, so --allow-untrusted.",
        },
      ];

    case 'goproxy':
      return [
        {
          title: 'Point the Go module proxy here',
          language: 'bash',
          code: `export GOPROXY=${proxy}
go mod download`,
          note: 'Append ,direct to fall back to VCS for modules this proxy does not cover.',
        },
      ];

    case 'puppet':
      return [
        {
          title: 'Install a module from the Forge proxy',
          language: 'bash',
          code: `puppet module install <author>-<module> \\
  --module_repository ${proxy}`,
        },
      ];

    case 'deb':
      return isLocal
        ? [
            {
              title: 'Add the apt repo (real apt repo, flat — Packages/Release auto-generated)',
              language: 'bash',
              code: `echo 'deb [trusted=yes] ${url}/api/v1/local/${name}/ ./' | sudo tee /etc/apt/sources.list.d/${name}.list
sudo apt-get update
sudo apt-get install <package>`,
              note: '[trusted=yes]: artifactapi serves the flat repo unsigned (matches the rpm repo\'s gpgcheck=0). The `./` is the flat-repo suite — apt fetches Packages/Release from the repo root.',
            },
            {
              title: 'Publish a .deb (index regenerates automatically)',
              language: 'bash',
              code: `curl -fsSL --upload-file ./my-package_1.0_amd64.deb \\
  ${url}/api/v2/remotes/${name}/files/my-package_1.0_amd64.deb`,
            },
          ]
        : [
            {
              title: 'Add the apt repo (caching proxy)',
              language: 'bash',
              code: `echo 'deb ${proxy} <suite> <component>' | sudo tee /etc/apt/sources.list.d/${name}.list
sudo apt-get update
sudo apt-get install <package>`,
              note: "Signatures are verified against the upstream mirror's real signed Release through the proxy (no [trusted=yes] needed). Example suite/component: bookworm main.",
            },
          ];

    case 'github_deb':
      return [
        {
          title: 'Add the apt repo (metadata-only, from GitHub releases)',
          language: 'bash',
          code: `echo 'deb [trusted=yes] ${proxy}/ ./' | sudo tee /etc/apt/sources.list.d/${name}.list
sudo apt-get update
sudo apt-get install <package>`,
          note: "The apt index is synthesized from the configured GitHub repo's release .deb assets; package downloads are redirected to the backing releases remote. Served unsigned, so [trusted=yes].",
        },
      ];

    case 'generic':
    default:
      return [
        {
          title: 'Download a file',
          language: 'bash',
          code: `curl -fsSLO ${proxy}/<path>`,
          note:
            packageType === 'generic'
              ? 'Generic repos are fetched as plain files at their upstream path.'
              : `No tailored client instructions for "${packageType}" yet — fetch artifacts directly by path.`,
        },
        ...(isLocal
          ? [
              {
                title: 'Publish a file',
                language: 'bash',
                code: `curl -fsSL --upload-file ./myfile \\
  ${url}/api/v2/remotes/${name}/files/<path>/myfile`,
              },
            ]
          : []),
      ];
  }
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
