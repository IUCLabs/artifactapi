import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'default',
  snippets: ({ packageType, name, url, proxy, isLocal }) => {
    return [
      {
        title: 'Download a file',
        language: 'bash',
        code: `curl -fsSLO ${proxy}/<path>`,
        note:
          packageType === 'generic'
            ? 'Generic repos are fetched as plain files at their upstream path.'
            : `No tailored client instructions for "${packageType}" yet — fetch artifacts directly by path.`,
      },
      ...(isLocal
        ? [
            {
              title: 'Publish a file',
              language: 'bash',
              code: `curl -fsSL --upload-file ./myfile \\
  ${url}/api/v2/remotes/${name}/files/<path>/myfile`,
            },
          ]
        : []),
    ];
  },
};

export default def;
