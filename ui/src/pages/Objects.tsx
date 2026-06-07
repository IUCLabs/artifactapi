import { useEffect, useState, useCallback } from 'react';
import { useParams, Link } from 'react-router-dom';
import { api } from '../api/client';
import type { Artifact } from '../api/types';
import { DataTable } from '../components/DataTable';
import { formatBytes, timeAgo, truncateHash } from '../components/format';
import './Objects.css';

export function Objects() {
  const { name } = useParams<{ name: string }>();
  const [artifacts, setArtifacts] = useState<Artifact[]>([]);
  const [page, setPage] = useState(1);
  const [loading, setLoading] = useState(true);
  const [filter, setFilter] = useState('');

  const load = useCallback(() => {
    if (!name) return;
    setLoading(true);
    api.listObjects(name, page, 50)
      .then(a => setArtifacts(a || []))
      .finally(() => setLoading(false));
  }, [name, page]);

  useEffect(() => { load(); }, [load]);

  const handleEvict = async (path: string) => {
    if (!name || !confirm(`Evict ${path}?`)) return;
    await api.evictObject(name, path);
    load();
  };

  const filtered = filter
    ? artifacts.filter(a => a.path.toLowerCase().includes(filter.toLowerCase()))
    : artifacts;

  return (
    <div>
      <div className="detail-header">
        <Link to={`/remotes/${name}`} className="back-link">&larr; {name}</Link>
        <h1 className="page-title">Cached Objects</h1>
      </div>

      <div className="objects-toolbar">
        <input
          className="search-input"
          placeholder="Filter by path..."
          value={filter}
          onChange={e => setFilter(e.target.value)}
        />
        <span className="result-count">{filtered.length} objects</span>
      </div>

      {loading ? (
        <div className="loading">Loading...</div>
      ) : (
        <>
          <DataTable
            columns={[
              {
                key: 'path',
                header: 'Path',
                render: (a: Artifact) => <span className="mono obj-path">{a.path}</span>,
              },
              {
                key: 'size',
                header: 'Size',
                render: (a: Artifact) => formatBytes(a.size_bytes),
                width: '100px',
              },
              {
                key: 'hash',
                header: 'Hash',
                render: (a: Artifact) => (
                  <span className="mono hash-cell" title={a.content_hash}>
                    {truncateHash(a.content_hash)}
                  </span>
                ),
                width: '160px',
              },
              {
                key: 'accessed',
                header: 'Last Accessed',
                render: (a: Artifact) => timeAgo(a.last_accessed_at),
                width: '120px',
              },
              {
                key: 'hits',
                header: 'Hits',
                render: (a: Artifact) => a.access_count,
                width: '70px',
              },
              {
                key: 'actions',
                header: '',
                render: (a: Artifact) => (
                  <button
                    className="btn-evict"
                    onClick={(e) => { e.stopPropagation(); handleEvict(a.path); }}
                  >
                    Evict
                  </button>
                ),
                width: '80px',
              },
            ]}
            data={filtered}
            emptyMessage="No cached objects"
          />

          <div className="pagination">
            <button
              className="btn btn-sm"
              disabled={page === 1}
              onClick={() => setPage(p => Math.max(1, p - 1))}
            >
              Previous
            </button>
            <span className="page-info">Page {page}</span>
            <button
              className="btn btn-sm"
              disabled={artifacts.length < 50}
              onClick={() => setPage(p => p + 1)}
            >
              Next
            </button>
          </div>
        </>
      )}
    </div>
  );
}
