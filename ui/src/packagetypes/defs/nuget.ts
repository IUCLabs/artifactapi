import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'blue',
  snippets: ({ name, proxy }) => {
    return [
      {
        title: 'Use this feed instead of nuget.org',
        language: 'xml',
        code: `<!-- nuget.config -->
<configuration>
  <packageSources>
    <clear />
    <add key="${name}" value="${proxy}/index.json" protocolVersion="3" />
  </packageSources>
</configuration>`,
        note: 'Serve artifactapi over HTTPS: NuGet rejects the repository signatures resource over plain HTTP.',
      },
    ];
  },
};

export default def;
