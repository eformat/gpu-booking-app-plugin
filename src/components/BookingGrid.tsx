import * as React from 'react';
import { Button, Label } from '@patternfly/react-core';
import { MicrochipIcon } from '@patternfly/react-icons';
import {
  Table,
  Thead,
  Tr,
  Th,
  Tbody,
  Td,
} from '@patternfly/react-table';
import {
  Booking,
  GPUResource,
  SLOT_TYPE,
  isPastDate,
  isWeekend,
  todayStr,
  formatHour,
} from '../utils/constants';

interface BookingGridProps {
  resource: GPUResource;
  dates: string[];
  bookings: Booking[];
  activeReservations: Record<string, string>;
  currentUser: string;
  reservingKey: string | null;
  confirmCancelId: string | null;
  onReserve: (resource: string, slotIndex: number, date: string, slotType: string) => void;
  onCancel: (id: string) => void;
  onEdit: (booking: Booking) => void;
  onConfirmCancel: (id: string | null) => void;
}

const BookingGrid: React.FC<BookingGridProps> = ({
  resource,
  dates,
  bookings,
  activeReservations,
  currentUser,
  reservingKey,
  confirmCancelId,
  onReserve,
  onCancel,
  onEdit,
  onConfirmCancel,
}) => {
  const getBooking = (unitIdx: number, date: string): Booking | undefined => {
    return bookings.find(
      (b) => b.resource === resource.type && b.slotIndex === unitIdx && b.date === date && b.slotType === SLOT_TYPE,
    );
  };

  return (
    <div style={{ marginBottom: '24px', border: '1px solid var(--pf-t--global--border--color--default)', borderRadius: '8px', overflow: 'hidden' }}>
      <div style={{
        padding: '12px 16px',
        backgroundColor: 'var(--pf-t--global--background--color--secondary--default)',
        borderBottom: '1px solid var(--pf-t--global--border--color--default)',
        display: 'flex',
        alignItems: 'center',
        gap: '8px',
      }}>
        <MicrochipIcon />
        <span style={{ fontWeight: 600 }}>{resource.name}</span>
        <span style={{ fontSize: '14px', color: 'var(--pf-t--global--text--color--regular)', opacity: 0.8 }}>
          ({resource.count} units)
        </span>
      </div>

      <div style={{ overflowX: 'auto' }}>
        <Table aria-label={`${resource.name} booking grid`} variant="compact">
          <Thead>
            <Tr>
              <Th style={{ minWidth: '180px' }}>Date / Slot</Th>
              {Array.from({ length: resource.count }, (_, i) => (
                <Th key={i} style={{ minWidth: '120px', textAlign: 'center' }}>
                  Unit {i + 1}
                </Th>
              ))}
            </Tr>
          </Thead>
          <Tbody>
            {dates.map((date) => {
              const dateObj = new Date(date + 'T00:00:00');
              const today = date === todayStr();
              const weekend = isWeekend(date);
              const past = isPastDate(date);
              const fullDisplay = dateObj.toLocaleDateString('en-GB', {
                weekday: 'long',
                day: 'numeric',
                month: 'long',
              });

              const dateBookingCount = bookings.filter(
                (b) => b.resource === resource.type && b.date === date && b.slotType === SLOT_TYPE,
              ).length;

              let headerBg = '#383838';
              if (today) headerBg = '#EE0000';
              else if (past) headerBg = '#6A6A6A';
              else if (weekend) headerBg = '#8A8A8A';

              return (
                <React.Fragment key={date}>
                  <Tr style={{ backgroundColor: headerBg }}>
                    <Td colSpan={resource.count + 1} style={{ padding: '8px 16px' }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
                        <span style={{ color: 'white', fontWeight: 'bold', fontSize: '14px' }}>
                          {fullDisplay}
                        </span>
                        {today && (
                          <Label color="red" isCompact>TODAY</Label>
                        )}
                        {past && (
                          <Label color="grey" isCompact>HISTORY</Label>
                        )}
                        {!past && weekend && (
                          <Label color="grey" isCompact>WEEKEND</Label>
                        )}
                      </div>
                    </Td>
                  </Tr>
                  <Tr style={{ backgroundColor: weekend ? 'var(--pf-t--global--background--color--secondary--default)' : undefined }}>
                    <Td>
                      <div style={{ display: 'flex', alignItems: 'center', gap: '4px', fontSize: '12px', color: 'var(--pf-t--global--text--color--regular)' }}>
                        <MicrochipIcon />
                        <span style={{
                          fontWeight: 500,
                          color: resource.count - dateBookingCount === 0
                            ? '#c9190b'
                            : '#1e7e34',
                        }}>
                          {resource.count - dateBookingCount}
                        </span>
                        <span>/ {resource.count} available</span>
                      </div>
                    </Td>
                    {Array.from({ length: resource.count }, (_, unitIdx) => {
                      const booking = getBooking(unitIdx, date);
                      const cellKey = `${resource.type}-${unitIdx}-${date}-${SLOT_TYPE}`;
                      const isReserving = reservingKey === cellKey;

                      return (
                        <Td key={unitIdx} style={{ textAlign: 'center', opacity: past ? 0.7 : 1 }}>
                          {booking ? (
                            <div title={booking.description || undefined}>
                              <div style={{ fontSize: '13px', fontWeight: 500 }}>
                                {booking.user}
                              </div>
                              {booking.description && (
                                <div style={{ fontSize: '10px', color: 'var(--pf-t--global--text--color--regular)', opacity: 0.7, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', maxWidth: '100px', margin: '0 auto' }}>
                                  {booking.description}
                                </div>
                              )}
                              {(booking.startHour !== 0 || booking.endHour !== 24) && (
                                <div style={{ fontSize: '10px', color: 'var(--pf-t--global--text--color--regular)', opacity: 0.7 }}>
                                  {formatHour(booking.startHour)}&mdash;{booking.endHour === 24 ? '00:00' : formatHour(booking.endHour)} UTC
                                </div>
                              )}
                              {past ? (
                                <span style={{ fontSize: '10px', color: 'var(--pf-t--global--text--color--regular)', opacity: 0.7 }}>
                                  {booking.source === 'consumed' ? '\u26A1 consumed' : activeReservations[booking.user] ? `\uD83D\uDC0D ${activeReservations[booking.user]}` : ''}
                                </span>
                              ) : booking.source === 'consumed' ? (
                                <div>
                                  <div style={{ fontSize: '11px', color: '#0066cc', fontWeight: 500 }}>
                                    {'\u26A1'} consumed
                                  </div>
                                  <Button
                                    variant="warning"
                                    size="sm"
                                    onClick={() => onReserve(resource.type, unitIdx, date, SLOT_TYPE)}
                                    isDisabled={isReserving}
                                    style={{ marginTop: '4px' }}
                                  >
                                    {isReserving ? '...' : 'Override'}
                                  </Button>
                                </div>
                              ) : booking.user === currentUser || activeReservations[booking.user] ? (
                                <div>
                                  {activeReservations[booking.user] && (
                                    <div style={{ fontSize: '11px', color: '#1e7e34', fontWeight: 500 }}>
                                      {'\uD83D\uDC0D'} {activeReservations[booking.user]}
                                    </div>
                                  )}
                                  {confirmCancelId === booking.id ? (
                                    <div style={{ display: 'flex', gap: '4px', marginTop: '4px', justifyContent: 'center' }}>
                                      <Button variant="danger" size="sm" onClick={() => onCancel(booking.id)}>
                                        Confirm
                                      </Button>
                                      <Button variant="secondary" size="sm" onClick={() => onConfirmCancel(null)}>
                                        No
                                      </Button>
                                    </div>
                                  ) : booking.user === currentUser ? (
                                    <div style={{ display: 'flex', gap: '4px', marginTop: '4px', justifyContent: 'center' }}>
                                      <Button variant="link" size="sm" onClick={() => onEdit(booking)}>
                                        Edit
                                      </Button>
                                      <Button variant="link" size="sm" isDanger onClick={() => onConfirmCancel(booking.id)}>
                                        Cancel
                                      </Button>
                                    </div>
                                  ) : null}
                                </div>
                              ) : (
                                confirmCancelId === booking.id ? (
                                  <div style={{ display: 'flex', gap: '4px', marginTop: '4px', justifyContent: 'center' }}>
                                    <Button variant="danger" size="sm" onClick={() => onCancel(booking.id)}>
                                      Confirm
                                    </Button>
                                    <Button variant="secondary" size="sm" onClick={() => onConfirmCancel(null)}>
                                      No
                                    </Button>
                                  </div>
                                ) : null
                              )}
                            </div>
                          ) : past ? (
                            <span style={{ fontSize: '12px', color: 'var(--pf-t--global--text--color--regular)', opacity: 0.7 }}>&mdash;</span>
                          ) : (
                            <Button
                              variant="primary"
                              size="sm"
                              onClick={() => onReserve(resource.type, unitIdx, date, SLOT_TYPE)}
                              isDisabled={isReserving}
                            >
                              {isReserving ? '...' : 'Reserve'}
                            </Button>
                          )}
                        </Td>
                      );
                    })}
                  </Tr>
                </React.Fragment>
              );
            })}
          </Tbody>
        </Table>
      </div>
    </div>
  );
};

export default BookingGrid;
