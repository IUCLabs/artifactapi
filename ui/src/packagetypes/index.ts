import type { BadgeColor, PackageTypeUI } from './types';

const defs = import.meta.glob<PackageTypeUI>('./defs/*.ts', { eager: true, import: 'default' });

export const packageTypes: Record<string, PackageTypeUI> = Object.fromEntries(
  Object.entries(defs).map(([path, def]) => [path.slice('./defs/'.length, -'.ts'.length), def]),
);

export function typeColor(packageType: string): BadgeColor {
  return packageTypes[packageType]?.color || 'default';
}
