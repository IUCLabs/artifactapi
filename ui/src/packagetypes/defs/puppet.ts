import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'yellow',
  snippets: ({ name, url, proxy, isLocal }) => {
    return [
      {
        title: isLocal ? 'Install a module from this forge' : 'Install a module from the Forge proxy',
        language: 'bash',
        code: `puppet module install <author>-<module> \\
  --module_repository ${isLocal ? `${url}/api/v1/local/${name}` : proxy}`,
      },
      ...(isLocal
        ? [
            {
              title: 'Publish a module (versions are immutable)',
              language: 'bash',
              code: `puppet module build && curl -fsSL --upload-file pkg/<author>-<module>-<version>.tar.gz \\
  ${url}/api/v2/remotes/${name}/files/<author>-<module>-<version>.tar.gz`,
            },
          ]
        : []),
    ];
  },
};

export default def;
