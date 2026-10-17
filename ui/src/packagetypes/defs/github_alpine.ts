import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  snippets: ({ proxy }) => {
    return [
      {
        title: 'Add the apk repo (metadata-only, from GitHub releases)',
        language: 'bash',
        code: `echo '${proxy}' | sudo tee -a /etc/apk/repositories
sudo apk update --allow-untrusted
sudo apk add --allow-untrusted <package>`,
        note: "The per-arch APKINDEX is synthesized from the configured GitHub repo's release .apk assets; package downloads are redirected to the backing releases remote. Served unsigned, so --allow-untrusted.",
      },
    ];
  },
};

export default def;
