import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  snippets: ({ name, url, proxy, isLocal }) => {
    return isLocal
      ? [
          {
            title: 'Add the apt repo (real apt repo, flat — Packages/Release auto-generated)',
            language: 'bash',
            code: `echo 'deb [trusted=yes] ${url}/api/v1/local/${name}/ ./' | sudo tee /etc/apt/sources.list.d/${name}.list
sudo apt-get update
sudo apt-get install <package>`,
            note: '[trusted=yes]: artifactapi serves the flat repo unsigned (matches the rpm repo\'s gpgcheck=0). The `./` is the flat-repo suite — apt fetches Packages/Release from the repo root.',
          },
          {
            title: 'Publish a .deb (index regenerates automatically)',
            language: 'bash',
            code: `curl -fsSL --upload-file ./my-package_1.0_amd64.deb \\
  ${url}/api/v2/remotes/${name}/files/my-package_1.0_amd64.deb`,
          },
        ]
      : [
          {
            title: 'Add the apt repo (caching proxy)',
            language: 'bash',
            code: `echo 'deb ${proxy} <suite> <component>' | sudo tee /etc/apt/sources.list.d/${name}.list
sudo apt-get update
sudo apt-get install <package>`,
            note: "Signatures are verified against the upstream mirror's real signed Release through the proxy (no [trusted=yes] needed). Example suite/component: bookworm main.",
          },
        ];
  },
};

export default def;
