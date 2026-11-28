import type { Remote, Virtual, Artifact, OverviewStats, RemoteStatRow, FileStatRow, BandwidthStatRow, HealthStatus, ProbeResult, GitHubApp, GitHubAppManifestStart, GitHubAppManifestConverted } from './types';

const BASE = '';

const TOKEN_KEY = 'artifactapi.token';

// The write token lives in sessionStorage so it survives the GitHub redirects
// of the manifest flow but not the browser session.
export function getToken(): string {
  try {
    return sessionStorage.getItem(TOKEN_KEY) ?? '';
  } catch {
    return '';
  }
}

export function setToken(token: string) {
  try {
    if (token) sessionStorage.setItem(TOKEN_KEY, token);
    else sessionStorage.removeItem(TOKEN_KEY);
  } catch {
    // storage unavailable: the token only lasts for this page
  }
}

export class APIError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function fetchJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const token = getToken();
  const resp = await fetch(`${BASE}${path}`, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...init?.headers,
    },
  });
  if (!resp.ok) {
    const text = (await resp.text()).trim();
    throw new APIError(resp.status, `${resp.status}: ${text}`);
  }
  if (resp.status === 204) return undefined as T;
  return resp.json();
}

export const api = {
  health: () => fetchJSON<HealthStatus>('/api/v2/health'),
  stats: () => fetchJSON<OverviewStats>('/api/v2/stats'),
  topRemotes: () => fetchJSON<RemoteStatRow[]>('/api/v2/stats/top-remotes'),
  topFilesByHits: () => fetchJSON<FileStatRow[]>('/api/v2/stats/top-files-by-hits'),
  topFilesByBandwidth: () => fetchJSON<BandwidthStatRow[]>('/api/v2/stats/top-files-by-bandwidth'),

  listRemotes: () => fetchJSON<Remote[]>('/api/v2/remotes'),
  getRemote: (name: string) => fetchJSON<Remote>(`/api/v2/remotes/${name}`),

  listVirtuals: () => fetchJSON<Virtual[]>('/api/v2/virtuals'),
  getVirtual: (name: string) => fetchJSON<Virtual>(`/api/v2/virtuals/${name}`),

  listObjects: (remote: string, page = 1, perPage = 50) =>
    fetchJSON<Artifact[]>(`/api/v2/remotes/${remote}/objects?page=${page}&per_page=${perPage}`),

  evictObject: (remote: string, path: string) =>
    fetchJSON<void>(`/api/v2/remotes/${remote}/objects/${path}`, { method: 'DELETE' }),

  listLocalObjects: (name: string, page = 1, perPage = 50) =>
    fetchJSON<Artifact[]>(`/api/v2/locals/${name}/objects?page=${page}&per_page=${perPage}`),

  evictLocalObject: (name: string, path: string) =>
    fetchJSON<void>(`/api/v2/locals/${name}/objects/${path}`, { method: 'DELETE' }),

  flushRemoteCache: (remote: string) =>
    fetchJSON<void>(`/api/v2/remotes/${remote}/cache`, { method: 'DELETE' }),

  listGitHubApps: () => fetchJSON<GitHubApp[]>('/api/v2/github-apps'),

  startGitHubAppManifest: (body: { name: string; owner: string; github_host: string; github_app_name: string }) =>
    fetchJSON<GitHubAppManifestStart>('/api/v2/github-apps/manifest', {
      method: 'POST',
      body: JSON.stringify(body),
    }),

  convertGitHubAppManifest: (code: string, state: string) =>
    fetchJSON<GitHubAppManifestConverted>('/api/v2/github-apps/manifest/convert', {
      method: 'POST',
      body: JSON.stringify({ code, state }),
    }),

  githubAppInstalled: (installationID: number, state: string) =>
    fetchJSON<GitHubApp>('/api/v2/github-apps/manifest/installed', {
      method: 'POST',
      body: JSON.stringify({ installation_id: installationID, state }),
    }),

  probe: (remote: string, path: string) =>
    fetchJSON<ProbeResult>('/api/v2/probe', {
      method: 'POST',
      body: JSON.stringify({ remote, path }),
    }),
};
