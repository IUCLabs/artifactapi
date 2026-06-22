import type { Remote, Virtual, Artifact, OverviewStats, RemoteStatRow, FileStatRow, BandwidthStatRow, HealthStatus, ProbeResult } from './types';

const BASE = '';

async function fetchJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(`${BASE}${path}`, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...init?.headers },
  });
  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(`${resp.status}: ${text}`);
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

  flushRemoteCache: (remote: string) =>
    fetchJSON<void>(`/api/v2/remotes/${remote}/cache`, { method: 'DELETE' }),

  probe: (remote: string, path: string) =>
    fetchJSON<ProbeResult>('/api/v2/probe', {
      method: 'POST',
      body: JSON.stringify({ remote, path }),
    }),
};
