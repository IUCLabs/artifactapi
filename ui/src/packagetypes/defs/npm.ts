import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'red',
  snippets: ({ proxy }) => {
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
  },
};

export default def;
