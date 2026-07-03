import { useEffect, useState, useCallback, useMemo } from 'react';
import { useParams, useLocation, Link } from 'react-router-dom';
import { api } from '../api/client';
import type { Artifact } from '../api/types';
import { formatBytes, timeAgo, truncateHash } from '../components/format';
import './Objects.css';

interface TreeNode {
  name: string;
  path: string;
  children: Map<string, TreeNode>;
  artifact?: Artifact;
  totalSize: number;
  totalHits: number;
  fileCount: number;
}

function buildTree(artifacts: Artifact[]): TreeNode {
  const root: TreeNode = {
    name: '',
    path: '',
    children: new Map(),
    totalSize: 0,
    totalHits: 0,
    fileCount: 0,
  };

  for (const a of artifacts) {
    const parts = a.path.split('/').filter(Boolean);
    let node = root;
    let currentPath = '';

    for (let i = 0; i < parts.length; i++) {
      currentPath += (currentPath ? '/' : '') + parts[i];
      const isLeaf = i === parts.length - 1;

      if (!node.children.has(parts[i])) {
        node.children.set(parts[i], {
          name: parts[i],
          path: currentPath,
          children: new Map(),
          totalSize: 0,
          totalHits: 0,
          fileCount: 0,
        });
      }

      const child = node.children.get(parts[i])!;
      child.totalSize += a.size_bytes;
      child.totalHits += a.access_count;
      child.fileCount += 1;

      if (isLeaf) {
        child.artifact = a;
      }

      node = child;
    }

    root.totalSize += a.size_bytes;
    root.totalHits += a.access_count;
    root.fileCount += 1;
  }

  return root;
}

function filterTree(node: TreeNode, query: string): TreeNode | null {
  if (!query) return node;
  const lower = query.toLowerCase();

  if (node.artifact) {
    if (node.path.toLowerCase().includes(lower)) return node;
    return null;
  }

  const filtered: Map<string, TreeNode> = new Map();
  let totalSize = 0;
  let totalHits = 0;
  let fileCount = 0;

  for (const [key, child] of node.children) {
    const result = filterTree(child, query);
    if (result) {
      filtered.set(key, result);
      totalSize += result.totalSize;
      totalHits += result.totalHits;
      fileCount += result.fileCount;
    }
  }

  if (filtered.size === 0) return null;

  return { ...node, children: filtered, totalSize, totalHits, fileCount };
}

interface TreeRowProps {
  node: TreeNode;
  depth: number;
  expanded: Set<string>;
  onToggle: (path: string) => void;
  onEvict: (path: string) => void;
}

function TreeRow({ node, depth, expanded, onToggle, onEvict }: TreeRowProps) {
  const isDir = node.children.size > 0 && !node.artifact;
  const isExpanded = expanded.has(node.path);

  const sortedChildren = useMemo(() => {
    if (!isDir) return [];
    return Array.from(node.children.values()).sort((a, b) => {
      const aIsDir = a.children.size > 0 && !a.artifact;
      const bIsDir = b.children.size > 0 && !b.artifact;
      if (aIsDir !== bIsDir) return aIsDir ? -1 : 1;
      return a.name.localeCompare(b.name);
    });
  }, [node.children, isDir]);

  return (
    <>
      <tr className={isDir ? 'tree-dir' : 'tree-file'} onClick={isDir ? () => onToggle(node.path) : undefined}>
        <td>
          <span style={{ paddingLeft: depth * 20 }} className="tree-label">
            {isDir && (
              <span className="tree-toggle">{isExpanded ? '▾' : '▸'}</span>
            )}
            <span className={isDir ? 'tree-dir-name' : 'mono tree-file-name'}>
              {node.name}{isDir ? '/' : ''}
            </span>
          </span>
        </td>
        <td className="num-cell">{formatBytes(node.totalSize)}</td>
        {node.artifact ? (
          <>
            <td className="mono hash-cell" title={node.artifact.content_hash}>
              {truncateHash(node.artifact.content_hash)}
            </td>
            <td>{timeAgo(node.artifact.last_accessed_at)}</td>
            <td className="num-cell">{node.artifact.access_count}</td>
            <td>
              <button
                className="btn-evict"
                onClick={(e) => { e.stopPropagation(); onEvict(node.artifact!.path); }}
              >
                Evict
              </button>
            </td>
          </>
        ) : (
          <>
            <td className="tree-summary">{node.fileCount} files</td>
            <td></td>
            <td className="num-cell">{node.totalHits}</td>
            <td></td>
          </>
        )}
      </tr>
      {isDir && isExpanded && sortedChildren.map(child => (
        <TreeRow
          key={child.path}
          node={child}
          depth={depth + 1}
          expanded={expanded}
          onToggle={onToggle}
          onEvict={onEvict}
        />
      ))}
    </>
  );
}

export function Objects() {
  const { name } = useParams<{ name: string }>();
  const location = useLocation();
  const isLocal = location.pathname.startsWith('/locals/');
  const backLink = isLocal ? `/locals/${name}` : `/remotes/${name}`;
  const [artifacts, setArtifacts] = useState<Artifact[]>([]);
  const [loading, setLoading] = useState(true);
  const [filter, setFilter] = useState('');
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  const load = useCallback(() => {
    if (!name) return;
    setLoading(true);
    const req = isLocal ? api.listLocalObjects(name, 1, 5000) : api.listObjects(name, 1, 5000);
    req
      .then(a => setArtifacts(a || []))
      .finally(() => setLoading(false));
  }, [name, isLocal]);

  useEffect(() => { load(); }, [load]);

  const handleEvict = async (path: string) => {
    if (!name || !confirm(`Evict ${path}?`)) return;
    await (isLocal ? api.evictLocalObject(name, path) : api.evictObject(name, path));
    load();
  };

  const tree = useMemo(() => buildTree(artifacts), [artifacts]);
  const filtered = useMemo(() => filterTree(tree, filter), [tree, filter]);

  const toggleExpand = useCallback((path: string) => {
    setExpanded(prev => {
      const next = new Set(prev);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });
  }, []);

  const expandAll = useCallback(() => {
    const allDirs = new Set<string>();
    function walk(node: TreeNode) {
      if (node.children.size > 0 && !node.artifact) {
        allDirs.add(node.path);
        for (const child of node.children.values()) walk(child);
      }
    }
    if (filtered) walk(filtered);
    setExpanded(allDirs);
  }, [filtered]);

  const collapseAll = useCallback(() => {
    setExpanded(new Set());
  }, []);

  const topChildren = useMemo(() => {
    if (!filtered) return [];
    return Array.from(filtered.children.values()).sort((a, b) => {
      const aIsDir = a.children.size > 0 && !a.artifact;
      const bIsDir = b.children.size > 0 && !b.artifact;
      if (aIsDir !== bIsDir) return aIsDir ? -1 : 1;
      return a.name.localeCompare(b.name);
    });
  }, [filtered]);

  return (
    <div>
      <div className="detail-header">
        <Link to={backLink} className="back-link">&larr; {name}</Link>
        <h1 className="page-title">Cached Objects</h1>
      </div>

      <div className="objects-toolbar">
        <input
          className="search-input"
          placeholder="Filter by path..."
          value={filter}
          onChange={e => setFilter(e.target.value)}
        />
        <span className="result-count">
          {filtered ? filtered.fileCount : 0} objects
          {filtered && filtered.fileCount > 0 && <> &middot; {formatBytes(filtered.totalSize)}</>}
        </span>
        <div className="tree-controls">
          <button className="btn btn-sm" onClick={expandAll}>Expand All</button>
          <button className="btn btn-sm" onClick={collapseAll}>Collapse All</button>
        </div>
      </div>

      {loading ? (
        <div className="loading">Loading...</div>
      ) : (
        <div className="data-table-wrap">
          <table className="data-table tree-table">
            <thead>
              <tr>
                <th>Path</th>
                <th style={{ width: 100 }}>Size</th>
                <th style={{ width: 160 }}>Hash</th>
                <th style={{ width: 120 }}>Last Accessed</th>
                <th style={{ width: 70 }}>Hits</th>
                <th style={{ width: 80 }}></th>
              </tr>
            </thead>
            <tbody>
              {topChildren.length === 0 ? (
                <tr>
                  <td colSpan={6} className="data-table-empty">No cached objects</td>
                </tr>
              ) : (
                topChildren.map(child => (
                  <TreeRow
                    key={child.path}
                    node={child}
                    depth={0}
                    expanded={expanded}
                    onToggle={toggleExpand}
                    onEvict={handleEvict}
                  />
                ))
              )}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
