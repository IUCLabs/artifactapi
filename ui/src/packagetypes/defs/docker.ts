import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'blue',
  snippets: ({ name, host, isLocal }) => {
    return [
      {
        title: 'Pull an image',
        language: 'bash',
        code: `docker pull ${host}/${name}/<image>:<tag>`,
        note: 'The first path segment after the host is the artifactapi repo name; the rest is the image name.',
      },
      ...(isLocal
        ? [
            {
              title: 'Push an image (this is a real Registry V2)',
              language: 'bash',
              code: `docker tag myapp:latest ${host}/${name}/myapp:latest
docker push        ${host}/${name}/myapp:latest`,
              note: 'Works with docker, podman, skopeo and buildah. If the registry requires auth, run `docker login ' + host + '` first.',
            },
          ]
        : []),
    ];
  },
};

export default def;
