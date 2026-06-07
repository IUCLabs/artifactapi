import { Routes, Route, NavLink } from 'react-router-dom';
import { Dashboard } from './pages/Dashboard';
import { Remotes } from './pages/Remotes';
import { RemoteDetail } from './pages/RemoteDetail';
import { Virtuals } from './pages/Virtuals';
import { Objects } from './pages/Objects';
import { Probe } from './pages/Probe';
import './App.css';

export function App() {
  return (
    <div className="app">
      <nav className="sidebar">
        <div className="sidebar-brand">
          <span className="brand-icon">&#x2B22;</span>
          <span className="brand-text">ArtifactAPI</span>
        </div>
        <div className="sidebar-nav">
          <NavLink to="/" end>Dashboard</NavLink>
          <NavLink to="/remotes">Remotes</NavLink>
          <NavLink to="/virtuals">Virtuals</NavLink>
          <NavLink to="/probe">Test Remote</NavLink>
        </div>
        <div className="sidebar-footer">
          <span className="version">v3.0-dev</span>
        </div>
      </nav>
      <main className="content">
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/remotes" element={<Remotes />} />
          <Route path="/remotes/:name" element={<RemoteDetail />} />
          <Route path="/remotes/:name/objects" element={<Objects />} />
          <Route path="/virtuals" element={<Virtuals />} />
          <Route path="/probe" element={<Probe />} />
        </Routes>
      </main>
    </div>
  );
}
