import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'red',
  snippets: ({ name, url, proxy, isLocal }) => {
    if (isLocal) {
      const src = `${url}/api/v1/local/${name}`;
      return [
        {
          title: 'Push and yank',
          language: 'bash',
          code: `GEM_HOST_API_KEY=unused gem push --host ${src} <gem>-<version>.gem
GEM_HOST_API_KEY=unused gem yank <gem> -v <version> --host ${src}`,
          note: 'gem push needs an API key; any value works. Pushing an existing version is rejected.',
        },
        {
          title: 'Install',
          language: 'bash',
          code: `gem install <gem> --source ${src}/
# Gemfile
source "${src}/"`,
        },
      ];
    }
    return [
      {
        title: 'Use this remote as the RubyGems source',
        language: 'bash',
        code: `gem sources --clear-all --add ${proxy}/
bundle config mirror.https://rubygems.org ${proxy}`,
      },
    ];
  },
};

export default def;
