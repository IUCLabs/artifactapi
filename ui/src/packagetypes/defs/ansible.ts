import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'red',
  snippets: ({ proxy }) => {
    return [
      {
        title: 'Install collections through this Galaxy proxy',
        language: 'bash',
        code: `ansible-galaxy collection install community.general -s ${proxy}/api/`,
      },
    ];
  },
};

export default def;
