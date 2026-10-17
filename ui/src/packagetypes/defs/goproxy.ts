import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'green',
  snippets: ({ proxy }) => {
    return [
      {
        title: 'Point the Go module proxy here',
        language: 'bash',
        code: `export GOPROXY=${proxy}
go mod download`,
        note: 'Append ,direct to fall back to VCS for modules this proxy does not cover.',
      },
    ];
  },
};

export default def;
