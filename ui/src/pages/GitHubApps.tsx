import { useEffect, useRef, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { api, APIError, clearToken, getToken, setToken } from '../api/client';
import type { GitHubApp, GitHubAppManifestConverted } from '../api/types';
import { Badge } from '../components/Badge';
import { DataTable } from '../components/DataTable';
import './GitHubApps.css';

function errorMessage(e: unknown): string {
  if (e instanceof APIError) {
    if (e.status === 401) return 'Not authorised: paste a valid API token above and try again.';
    if (e.status === 403) return 'This step was started by a different user; start again while signed in as yourself.';
    if (e.status === 409) return e.message.replace(/^409: /, '');
    if (/expired|already used/.test(e.message)) return 'This link has expired or was already used (each step is valid once, for 1 hour). Start again from the GitHub Apps page.';
    return e.message;
  }
  return e instanceof Error ? e.message : String(e);
}

function TokenField() {
  const [token, set] = useState(getToken());
  return (
    <div className="gha-row">
      <label htmlFor="gha-token">API token</label>
      <input
        id="gha-token"
        className="search-input gha-input"
        type="password"
        placeholder="OIDC bearer token (stopgap until UI login exists; only needed when writes require auth)"
        value={token}
        onChange={e => { set(e.target.value); setToken(e.target.value.trim()); }}
      />
      <button className="btn" onClick={() => { clearToken(); set(''); }} disabled={!token}>Clear token</button>
    </div>
  );
}

function submitManifest(postURL: string, manifest: Record<string, unknown>) {
  const form = document.createElement('form');
  form.method = 'POST';
  form.action = postURL;
  const input = document.createElement('input');
  input.type = 'hidden';
  input.name = 'manifest';
  input.value = JSON.stringify(manifest);
  form.appendChild(input);
  document.body.appendChild(form);
  form.submit();
}

export function GitHubApps() {
  const [apps, setApps] = useState<GitHubApp[]>([]);
  const [loading, setLoading] = useState(true);
  const [name, setName] = useState('');
  const [ghName, setGhName] = useState('');
  const [owner, setOwner] = useState('');
  const [host, setHost] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  useEffect(() => {
    api.listGitHubApps()
      .then(a => setApps(a || []))
      .catch(e => setError(errorMessage(e)))
      .finally(() => setLoading(false));
  }, []);

  const install = async (app: string) => {
    setError('');
    try {
      window.location.href = (await api.githubAppInstallURL(app)).install_url;
    } catch (e) {
      setError(errorMessage(e));
    }
  };

  const create = async () => {
    setBusy(true);
    setError('');
    try {
      const r = await api.startGitHubAppManifest({
        name: name.trim(),
        owner: owner.trim(),
        github_host: host.trim(),
        github_app_name: ghName.trim(),
      });
      submitManifest(r.post_url, r.manifest);
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
    }
  };

  return (
    <div>
      <h1 className="page-title">GitHub Apps</h1>

      {loading ? (
        <div className="loading">Loading...</div>
      ) : (
        <DataTable
          columns={[
            { key: 'name', header: 'Name', render: (a: GitHubApp) => <span className="mono">{a.name}</span> },
            { key: 'app_id', header: 'App ID', render: (a: GitHubApp) => <span className="mono">{a.app_id}</span>, width: '110px' },
            {
              key: 'installation_id',
              header: 'Installation',
              render: (a: GitHubApp) =>
                a.installation_id ? <span className="mono">{a.installation_id}</span> : (
                  <span className="gha-install">
                    <Badge variant="yellow">not installed</Badge>
                    <button className="btn btn-primary" onClick={() => install(a.name)}>Install</button>
                  </span>
                ),
              width: '220px',
            },
            { key: 'fingerprint', header: 'Key fingerprint', render: (a: GitHubApp) => <span className="mono gha-fp">{a.private_key_fingerprint}</span> },
          ]}
          data={apps}
          emptyMessage="No GitHub Apps"
        />
      )}

      <h2 className="gha-heading">Create GitHub App</h2>
      <p className="gha-description">
        Creates a GitHub App with read-only contents and metadata access and no webhook. You confirm it on GitHub,
        then install it; artifactapi stores the app id, private key and installation itself.
      </p>
      <div className="gha-form">
        <TokenField />
        <div className="gha-row">
          <label htmlFor="gha-name">Name</label>
          <input id="gha-name" className="search-input gha-input" placeholder="artifactapi name, e.g. ci"
            value={name} onChange={e => setName(e.target.value)} />
        </div>
        <div className="gha-row">
          <label htmlFor="gha-ghname">GitHub name</label>
          <input id="gha-ghname" className="search-input gha-input" maxLength={34}
            placeholder={`artifactapi-${name.trim() || '<name>'} (must be unique on GitHub)`}
            value={ghName} onChange={e => setGhName(e.target.value)} />
        </div>
        <div className="gha-row">
          <label htmlFor="gha-owner">Owner</label>
          <input id="gha-owner" className="search-input gha-input" placeholder="organization login (empty = your personal account)"
            value={owner} onChange={e => setOwner(e.target.value)} />
        </div>
        <div className="gha-row">
          <label htmlFor="gha-host">Host</label>
          <input id="gha-host" className="search-input gha-input" placeholder="github.com (or a GHES host from GITHUB_APP_API_HOSTS)"
            value={host} onChange={e => setHost(e.target.value)} />
        </div>
        {error && <div className="probe-error">{error}</div>}
        <div>
          <button className="btn btn-primary" onClick={create} disabled={busy || !name.trim()}>
            {busy ? 'Redirecting to GitHub...' : 'Create GitHub App'}
          </button>
        </div>
      </div>
    </div>
  );
}

function FlowError({ error }: { error: string }) {
  return (
    <div className="gha-form">
      <div className="probe-error">{error}</div>
      <div><Link to="/github-apps">Back to GitHub Apps</Link></div>
    </div>
  );
}

export function GitHubAppManifestCallback() {
  const [params] = useSearchParams();
  const code = params.get('code') ?? '';
  const state = params.get('state') ?? '';
  const [result, setResult] = useState<GitHubAppManifestConverted | null>(null);
  const [error, setError] = useState(code && state ? '' : 'GitHub did not return a code and state; start again.');

  // Each state is single use, so StrictMode's double effect must not resend it.
  const started = useRef(false);
  useEffect(() => {
    if (started.current || !code || !state) return;
    started.current = true;
    api.convertGitHubAppManifest(code, state).then(setResult).catch(e => setError(errorMessage(e)));
  }, [code, state]);

  return (
    <div>
      <h1 className="page-title">GitHub App created</h1>
      {error ? <FlowError error={error} /> : !result ? (
        <div className="loading">Saving the app...</div>
      ) : (
        <div className="gha-form">
          <p>
            Stored <span className="mono">{result.name}</span> (GitHub app <span className="mono">{result.slug}</span>,
            id <span className="mono">{result.app_id}</span>). Install it on the account or repositories it should read.
          </p>
          <div>
            <a className="btn btn-primary" href={result.install_url}>Install on GitHub</a>
          </div>
        </div>
      )}
    </div>
  );
}

export function GitHubAppManifestInstalled() {
  const [params] = useSearchParams();
  const installationID = Number(params.get('installation_id') ?? '');
  const state = params.get('state') ?? '';
  const valid = Number.isSafeInteger(installationID) && installationID > 0 && state !== '';
  const [app, setApp] = useState<GitHubApp | null>(null);
  const [error, setError] = useState(valid ? '' : 'GitHub did not return an installation id and state; start again.');

  const started = useRef(false);
  useEffect(() => {
    if (started.current || !valid) return;
    started.current = true;
    api.githubAppInstalled(installationID, state).then(setApp).catch(e => setError(errorMessage(e)));
  }, [valid, installationID, state]);

  const snippet = app
    ? `data "artifactapi_github_app" "x" {\n  name = "${app.name}"\n}\n\n# in a remote:\ngithub_app = data.artifactapi_github_app.x.name`
    : '';

  return (
    <div>
      <h1 className="page-title">GitHub App installed</h1>
      {error ? <FlowError error={error} /> : !app ? (
        <div className="loading">Verifying the installation...</div>
      ) : (
        <div className="gha-form">
          <p>
            <span className="mono">{app.name}</span> is installed (installation <span className="mono">{app.installation_id}</span>).
            Reference it from Terraform:
          </p>
          <pre className="mono gha-snippet">{snippet}</pre>
          <div><Link to="/github-apps">Back to GitHub Apps</Link></div>
        </div>
      )}
    </div>
  );
}
