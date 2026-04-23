import * as React from 'react';
import {
  ExpandableSection,
  Label,
} from '@patternfly/react-core';
import { WarningTriangleIcon } from '@patternfly/react-icons';
import { PreemptedWorkload } from '../utils/api';

interface PreemptionBannerProps {
  workloads: PreemptedWorkload[];
}

const PreemptionBanner: React.FC<PreemptionBannerProps> = ({ workloads }) => {
  const [isExpanded, setIsExpanded] = React.useState(false);

  if (workloads.length === 0) return null;

  const toggle = (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: '6px' }}>
      <WarningTriangleIcon color="var(--pf-t--global--color--nonstatus--orange--default)" />
      <span>{workloads.length} preempted workload{workloads.length !== 1 ? 's' : ''}</span>
    </span>
  );

  return (
    <div style={{
      marginBottom: '16px',
      border: '1px solid var(--pf-t--global--border--color--default)',
      borderRadius: '8px',
      padding: '8px 16px',
      backgroundColor: 'var(--pf-t--global--background--color--secondary--default)',
    }}>
      <ExpandableSection
        toggleContent={toggle}
        isExpanded={isExpanded}
        onToggle={(_e, expanded) => setIsExpanded(expanded)}
        isIndented
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: '8px', marginTop: '4px' }}>
          {workloads.map((w) => (
            <div
              key={`${w.namespace}-${w.name}`}
              style={{
                display: 'flex',
                alignItems: 'flex-start',
                gap: '8px',
                padding: '6px 8px',
                borderRadius: '4px',
                backgroundColor: 'var(--pf-t--global--background--color--primary--default)',
                fontSize: '13px',
              }}
            >
              <Label color="orange" isCompact>{w.reason}</Label>
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ fontWeight: 500 }}>
                  {w.owner}
                  <span style={{ fontWeight: 400, opacity: 0.7, marginLeft: '6px' }}>
                    {w.namespace}/{w.name}
                  </span>
                </div>
                <div style={{ fontSize: '12px', opacity: 0.7, marginTop: '2px', wordBreak: 'break-word' }}>
                  {w.message}
                </div>
              </div>
              <span style={{ fontSize: '11px', opacity: 0.6, whiteSpace: 'nowrap' }}>
                {formatTimestamp(w.timestamp)}
              </span>
            </div>
          ))}
        </div>
      </ExpandableSection>
    </div>
  );
};

function formatTimestamp(ts: string): string {
  if (!ts) return '';
  try {
    const d = new Date(ts);
    return d.toLocaleString(undefined, {
      month: 'short', day: 'numeric',
      hour: '2-digit', minute: '2-digit',
    });
  } catch {
    return ts;
  }
}

export default PreemptionBanner;
