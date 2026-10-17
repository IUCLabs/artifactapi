import type { PackageTypeUI } from '../types';

const def: PackageTypeUI = {
  color: 'red',
  snippets: ({ proxy }) => {
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
