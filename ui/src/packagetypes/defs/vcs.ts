import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'default',
  snippets: ({ proxy }) => {
    return [
      {
        title: 'Download a tag archive',
        language: 'bash',
        code: `curl -fsSLO ${proxy}/<owner>/<repo>/archive/refs/tags/<tag>.tar.gz`,
      },
      {
        title: 'Download a branch archive (redirects to its current commit)',
        language: 'bash',
        code: `curl -fsSL -o src.tar.gz ${proxy}/<owner>/<repo>/archive/refs/heads/<branch>.tar.gz`,
      },
      {
        title: 'Download a release asset',
        language: 'bash',
        code: `curl -fsSLO ${proxy}/<owner>/<repo>/releases/download/<tag>/<asset>`,
      },
    ];
  },
};

export default def;
