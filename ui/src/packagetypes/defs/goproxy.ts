import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'green',
  snippets: ({ name, url, proxy, isLocal }) => {
    if (isLocal) {
      return [
        {
          title: 'Fetch private modules from this proxy',
          language: 'bash',
          code: `export GOPROXY=${url}/api/v1/local/${name},https://proxy.golang.org,direct
export GONOSUMDB=myorg.example.net
go get myorg.example.net/hello@latest`,
          note: 'Set GONOSUMDB (or GOPRIVATE) to your module prefix: private modules are not in sum.golang.org.',
        },
        {
          title: 'Publish a module version',
          language: 'bash',
          code: `# build the zip with golang.org/x/mod/zip (CreateFromDir), or reuse
# the .zip/.mod/.info from $(go env GOMODCACHE)/cache/download after go mod download
MOD=myorg.example.net/hello VER=v0.1.0
for ext in zip mod info; do
  curl -fsSL --upload-file $VER.$ext \\
    ${url}/api/v2/remotes/${name}/files/$MOD/@v/$VER.$ext
done`,
          note: 'Paths use the case-encoded module path (uppercase as !lower). Zips must hold every file under module@version/ and are rejected otherwise; .info is optional.',
        },
      ];
    }
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
