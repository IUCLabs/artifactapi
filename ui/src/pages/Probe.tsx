import { useEffect, useState } from 'react';
import { api } from '../api/client';
import type { Remote, ProbeResult } from '../api/types';
import { Badge } from '../components/Badge';
import { formatBytes } from '../components/format';
import './Probe.css';

const PRESETS: Record<string, { remote: string; path: string }[]> = {
  'gitea-dl': [
    { remote: 'gitea-dl', path: 'gitea/1.23.7/gitea-1.23.7-linux-amd64' },
    { remote: 'gitea-dl', path: 'act_runner/0.2.11/act_runner-0.2.11-linux-amd64' },
  ],
  'github': [
    { remote: 'github', path: 'ducaale/xh/releases/download/v0.24.0/xh-v0.24.0-x86_64-unknown-linux-musl.tar.gz' },
    { remote: 'github', path: 'mikefarah/yq/releases/download/v4.45.4/yq_linux_amd64' },
    { remote: 'github', path: 'neovim/neovim-releases/releases/download/v0.11.2/nvim-linux-x86_64.tar.gz' },
  ],
  'hashicorp-releases': [
    { remote: 'hashicorp-releases', path: 'terraform/1.12.2/terraform_1.12.2_linux_amd64.zip' },
  ],
  'goproxy': [
    { remote: 'goproxy', path: 'golang.org/x/net/@v/list' },
    { remote: 'goproxy', path: 'golang.org/x/net/@v/v0.55.0.info' },
  ],
};

export function Probe() {
  const [remotes, setRemotes] = useState<Remote[]>([]);
  const [remote, setRemote] = useState('');
  const [path, setPath] = useState('');
  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState<ProbeResult | null>(null);
  const [history, setHistory] = useState<(ProbeResult & { remote: string; path: string })[]>([]);

  useEffect(() => {
    api.listRemotes().then(r => {
      setRemotes(r || []);
      if (r?.length && !remote) setRemote(r[0].name);
    });
  }, []);

  const runProbe = async () => {
    if (!remote || !path) return;
    setLoading(true);
    setResult(null);
    try {
      const r = await api.probe(remote, path);
      setResult(r);
      setHistory(prev => [{ ...r, remote, path }, ...prev].slice(0, 20));
    } catch (e: unknown) {
      setResult({
        status: 0,
        source: '',
        content_type: '',
        size_bytes: 0,
        headers: {},
        duration_ms: 0,
        error: e instanceof Error ? e.message : String(e),
      });
    } finally {
      setLoading(false);
    }
  };

  const applyPreset = (r: string, p: string) => {
    setRemote(r);
    setPath(p);
  };

  const remotePresets = PRESETS[remote] || [];

  return (
    <div>
      <h1 className="page-title">Test Remote</h1>
      <p className="probe-description">
        Probe a remote to test connectivity and caching. The file is fetched and cached but not sent to your browser.
      </p>

      <div className="probe-form">
        <div className="probe-row">
          <label>Remote</label>
          <select
            className="type-select probe-select"
            value={remote}
            onChange={e => setRemote(e.target.value)}
          >
            {remotes.map(r => (
              <option key={r.name} value={r.name}>
                {r.name} ({r.package_type})
              </option>
            ))}
          </select>
        </div>

        <div className="probe-row">
          <label>Path</label>
          <input
            className="search-input probe-input"
            placeholder="e.g. gitea/1.23.7/gitea-1.23.7-linux-amd64"
            value={path}
            onChange={e => setPath(e.target.value)}
            onKeyDown={e => e.key === 'Enter' && runProbe()}
          />
        </div>

        <button className="btn btn-primary" onClick={runProbe} disabled={loading || !path}>
          {loading ? 'Probing...' : 'Probe'}
        </button>
      </div>

      {remotePresets.length > 0 && (
        <div className="presets">
          <span className="presets-label">Quick tests:</span>
          {remotePresets.map((p, i) => (
            <button
              key={i}
              className="preset-btn"
              onClick={() => applyPreset(p.remote, p.path)}
            >
              {p.path.split('/').pop()}
            </button>
          ))}
        </div>
      )}

      {result && (
        <div className={`probe-result ${result.status === 200 ? 'probe-ok' : 'probe-err'}`}>
          <div className="probe-result-header">
            <Badge variant={result.status === 200 ? 'green' : 'red'}>
              {result.status}
            </Badge>
            <Badge variant={result.source === 'cache' ? 'blue' : 'yellow'}>
              {result.source || 'error'}
            </Badge>
            <span className="probe-duration">{result.duration_ms}ms</span>
          </div>

          {result.error ? (
            <div className="probe-error">{result.error}</div>
          ) : (
            <dl className="probe-dl">
              <dt>Content-Type</dt>
              <dd className="mono">{result.content_type}</dd>
              <dt>Size</dt>
              <dd className="mono">{formatBytes(result.size_bytes)} ({result.size_bytes.toLocaleString()} bytes)</dd>
              <dt>Source</dt>
              <dd>{result.source === 'cache' ? 'Served from cache (S3)' : 'Fetched from upstream'}</dd>
              <dt>Duration</dt>
              <dd className="mono">{result.duration_ms}ms</dd>
              {result.headers && Object.entries(result.headers).map(([k, v]) => (
                <div key={k} className="probe-header-row">
                  <dt className="mono">{k}</dt>
                  <dd className="mono">{v}</dd>
                </div>
              ))}
            </dl>
          )}
        </div>
      )}

      {history.length > 0 && (
        <div className="probe-history">
          <h3 className="section-label">History</h3>
          <table className="data-table">
            <thead>
              <tr>
                <th>Remote</th>
                <th>Path</th>
                <th>Status</th>
                <th>Source</th>
                <th>Size</th>
                <th>Time</th>
              </tr>
            </thead>
            <tbody>
              {history.map((h, i) => (
                <tr
                  key={i}
                  className="clickable"
                  onClick={() => applyPreset(h.remote, h.path)}
                >
                  <td className="mono">{h.remote}</td>
                  <td className="mono probe-path-cell">{h.path}</td>
                  <td>
                    <Badge variant={h.status === 200 ? 'green' : 'red'}>{h.status}</Badge>
                  </td>
                  <td>
                    <Badge variant={h.source === 'cache' ? 'blue' : 'yellow'}>
                      {h.source || 'err'}
                    </Badge>
                  </td>
                  <td className="mono">{formatBytes(h.size_bytes)}</td>
                  <td className="mono">{h.duration_ms}ms</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
