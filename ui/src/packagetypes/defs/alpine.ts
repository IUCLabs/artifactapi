import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'green',
  snippets: ({ name, url, proxy, isLocal }) => {
    return isLocal
      ? [
          {
            title: 'Add the apk repo (real apk repo, APKINDEX auto-generated)',
            language: 'bash',
            code: `echo '${url}/api/v1/local/${name}' | sudo tee -a /etc/apk/repositories
sudo apk update --allow-untrusted
sudo apk add --allow-untrusted <package>`,
            note: `Served unsigned (parity with the rpm repo's gpgcheck=0) — use --allow-untrusted, or install a signing key. apk fetches <arch>/APKINDEX.tar.gz under this base.`,
          },
          {
            title: 'Publish a .apk (index regenerates automatically)',
            language: 'bash',
            code: `curl -fsSL --upload-file ./mypkg-1.0-r0.apk \\
  ${url}/api/v2/remotes/${name}/files/x86_64/mypkg-1.0-r0.apk`,
            note: 'Upload each package at <arch>/<name>-<version>.apk — apk reconstructs that exact path from the index (APKINDEX carries no filename), so a mismatched path will 404 on install.',
          },
        ]
      : [
          {
            title: 'Add the APK repository',
            language: 'bash',
            code: `echo '${proxy}/' | sudo tee -a /etc/apk/repositories
sudo apk update
sudo apk add <package>`,
            note: 'If the index is unsigned over the proxy, add --allow-untrusted or install the signing key into /etc/apk/keys.',
          },
        ];
  },
};

export default def;
