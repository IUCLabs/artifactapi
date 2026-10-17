import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  snippets: ({ name, proxy }) => {
    return [
      {
        title: 'Add the apt repo (metadata-only, from GitHub releases)',
        language: 'bash',
        code: `echo 'deb [trusted=yes] ${proxy}/ ./' | sudo tee /etc/apt/sources.list.d/${name}.list
sudo apt-get update
sudo apt-get install <package>`,
        note: "The apt index is synthesized from the configured GitHub repo's release .deb assets; package downloads are redirected to the backing releases remote. Served unsigned, so [trusted=yes].",
      },
    ];
  },
};

export default def;
