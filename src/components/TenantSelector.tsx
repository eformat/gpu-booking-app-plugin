import * as React from 'react';
import { Select, SelectOption, MenuToggle } from '@patternfly/react-core';

const STORAGE_KEY = 'gpu-booking-tenant';

interface TenantSelectorProps {
  tenants: string[];
  selected: string;
  onSelect: (tenant: string) => void;
}

export const TenantSelector: React.FC<TenantSelectorProps> = ({ tenants, selected, onSelect }) => {
  const [open, setOpen] = React.useState(false);

  if (tenants.length <= 1) return null;

  return (
    <Select
      isOpen={open}
      selected={selected}
      onSelect={(_ev, value) => {
        if (typeof value === 'string') {
          onSelect(value);
          localStorage.setItem(STORAGE_KEY, value);
        }
        setOpen(false);
      }}
      onOpenChange={setOpen}
      toggle={(ref) => (
        <MenuToggle ref={ref} onClick={() => setOpen(!open)} isExpanded={open}>
          {selected || 'Select tenant'}
        </MenuToggle>
      )}
    >
      {tenants.map((t) => (
        <SelectOption key={t} value={t}>
          {t}
        </SelectOption>
      ))}
    </Select>
  );
};

export function useSelectedTenant(tenants: string[], defaultTenant: string): [string, (t: string) => void] {
  const [selected, setSelected] = React.useState<string>(() => {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (stored && tenants.includes(stored)) return stored;
    return defaultTenant || tenants[0] || '';
  });

  // Sync if tenants list changes after initial load
  React.useEffect(() => {
    if (tenants.length > 0 && !tenants.includes(selected)) {
      const fallback = defaultTenant || tenants[0];
      setSelected(fallback);
      localStorage.setItem(STORAGE_KEY, fallback);
    }
  }, [tenants, defaultTenant]);

  return [selected, setSelected];
}
