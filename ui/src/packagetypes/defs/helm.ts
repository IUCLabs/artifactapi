import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'green',
  snippets: ({ name, url, proxy, isLocal }) => {
    return [
      {
        title: isLocal ? 'Add the Helm repo (index.yaml auto-generated)' : 'Add the Helm repo',
        language: 'bash',
        code: `helm repo add ${name} ${isLocal ? `${url}/api/v1/local/${name}` : proxy}/
helm repo update
helm install <release> ${name}/<chart>`,
      },
      ...(isLocal
        ? [
            {
              title: 'Publish a chart',
              language: 'bash',
              code: `helm package ./mychart
curl -fsSL --upload-file mychart-0.1.0.tgz \\
  ${url}/api/v2/remotes/${name}/files/mychart-0.1.0.tgz`,
            },
          ]
        : []),
    ];
  },
};

export default def;
