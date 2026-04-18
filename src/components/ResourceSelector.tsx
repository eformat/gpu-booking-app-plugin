import * as React from 'react';
import { Gallery, GalleryItem, Card, CardBody } from '@patternfly/react-core';
import { MicrochipIcon } from '@patternfly/react-icons';
import { GPUResource } from '../utils/constants';

interface ResourceSelectorProps {
  resources: GPUResource[];
  selectedResources: string[];
  onSelectionChange: (selected: string[]) => void;
}

const ResourceSelector: React.FC<ResourceSelectorProps> = ({
  resources,
  selectedResources,
  onSelectionChange,
}) => {
  const handleClick = (type: string, e: React.MouseEvent) => {
    if (e.ctrlKey || e.metaKey) {
      if (selectedResources.includes(type)) {
        if (selectedResources.length > 1) {
          onSelectionChange(selectedResources.filter((t) => t !== type));
        }
      } else {
        onSelectionChange([...selectedResources, type]);
      }
    } else {
      onSelectionChange([type]);
    }
  };

  return (
    <Gallery hasGutter minWidths={{ default: '200px' }}>
      {resources.map((r) => {
        const isSelected = selectedResources.includes(r.type);
        return (
          <GalleryItem key={r.type}>
            <Card
              isClickable
              style={{
                cursor: 'pointer',
                borderColor: isSelected ? 'var(--pf-t--global--color--brand--default)' : undefined,
                boxShadow: isSelected ? '0 0 0 2px rgba(0, 102, 204, 0.3)' : undefined,
                background: isSelected ? 'var(--pf-t--global--background--color--brand--default)' : undefined,
              }}
              onClick={(e) => handleClick(r.type, e as unknown as React.MouseEvent)}
            >
              <CardBody>
                <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '4px' }}>
                  <MicrochipIcon />
                  <span style={{ fontWeight: 600, fontSize: '14px' }}>{r.name}</span>
                </div>
                <div style={{ fontSize: '12px', color: 'var(--pf-t--global--text--color--regular)', opacity: 0.7 }}>
                  {r.count} units available per slot
                </div>
              </CardBody>
            </Card>
          </GalleryItem>
        );
      })}
    </Gallery>
  );
};

export default ResourceSelector;
