import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'blue',
  snippets: ({ name, url, host, isLocal }) => {
    return [
      {
        title: 'Use as a provider source (bare address, no mirror config)',
        language: 'hcl',
        code: `terraform {
  required_providers {
    ${name} = {
      source  = "${host}/${name}/<type>"
      version = ">= 0.1.0"
    }
  }
}`,
        note: 'The namespace segment is this repo name; <type> is the provider type. artifactapi signs SHA256SUMS server-side with its GPG key, so `terraform init` installs with no .terraformrc.',
      },
      ...(isLocal
        ? [
            {
              title: 'Publish a provider build',
              language: 'bash',
              code: `curl -fsSL --upload-file terraform-provider-<type>_0.1.0_linux_amd64.zip \\
  ${url}/api/v2/remotes/${name}/files/${name}/<type>/terraform-provider-<type>_0.1.0_linux_amd64.zip`,
            },
          ]
        : []),
    ];
  },
};

export default def;
