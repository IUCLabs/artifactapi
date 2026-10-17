import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'yellow',
  snippets: ({ name, url, proxy, isLocal }) => {
    return [
      {
        title: isLocal
          ? 'Add the yum repo (real yum repo, repodata auto-regenerated)'
          : 'Add the yum repo (caching proxy)',
        language: 'bash',
        code: `sudo tee /etc/yum.repos.d/${name}.repo >/dev/null <<'EOF'
[${name}]
name=${name} (artifactapi)
baseurl=${isLocal ? `${url}/api/v2/remotes/${name}/files/` : `${proxy}/`}
enabled=1
gpgcheck=0
repo_gpgcheck=0
EOF

sudo dnf install <package>`,
        note: isLocal
          ? 'gpgcheck=0: artifactapi serves the repo unsigned. If you sign your RPMs, import your key and set gpgcheck=1.'
          : 'gpgcheck=0 trusts upstream over the proxy. To verify package signatures, import the upstream GPG key and set gpgcheck=1.',
      },
      ...(isLocal
        ? [
            {
              title: 'Publish an RPM (repodata regenerates automatically)',
              language: 'bash',
              code: `curl -fsSL --upload-file ./my-package-1.0-1.el9.x86_64.rpm \\
  ${url}/api/v2/remotes/${name}/files/my-package-1.0-1.el9.x86_64.rpm`,
            },
          ]
        : []),
    ];
  },
};

export default def;
