import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'red',
  snippets: ({ name, url, proxy, isLocal }) => {
    if (isLocal) {
      const src = `${url}/api/v1/local/${name}/`;
      return [
        {
          title: 'Publish and install',
          language: 'bash',
          code: `ansible-galaxy collection publish <ns>-<name>-<version>.tar.gz --server ${src} --api-key unused
ansible-galaxy collection install <ns>.<name> --server ${src}`,
          note: 'publish needs an API key; any value works. Publishing an existing version is rejected.',
        },
      ];
    }
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
