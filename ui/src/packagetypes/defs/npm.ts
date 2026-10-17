import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'red',
  snippets: ({ name, url, host, proxy, isLocal }) => {
    if (isLocal) {
      const reg = `${url}/api/v1/local/${name}/`;
      return [
        {
          title: 'Configure .npmrc (registry + publish token)',
          language: 'bash',
          code: `cat >> .npmrc <<'EOF'
registry=${reg}
//${host}/api/v1/local/${name}/:_authToken=unused
EOF`,
          note: 'npm refuses to publish without an _authToken for the registry; any value works. For a scope only, use @<scope>:registry=… instead of registry=.',
        },
        {
          title: 'Publish and install',
          language: 'bash',
          code: `npm publish
npm install <package>`,
          note: 'Versions are immutable: republishing an existing version is rejected. The dist-tags sent with the publish (latest, or --tag) move to the new version.',
        },
      ];
    }
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
