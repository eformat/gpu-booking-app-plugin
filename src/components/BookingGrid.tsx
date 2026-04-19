import * as React from 'react';
import { Label } from '@patternfly/react-core';
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
} from '../utils/constants';
import BookingCell from './BookingCell';

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
                    {Array.from({ length: resource.count }, (_, unitIdx) => (
                      <BookingCell
                        key={unitIdx}
                        booking={getBooking(unitIdx, date)}
                        resourceType={resource.type}
                        unitIdx={unitIdx}
                        date={date}
                        past={past}
                        activeReservations={activeReservations}
                        currentUser={currentUser}
                        reservingKey={reservingKey}
                        confirmCancelId={confirmCancelId}
                        onReserve={onReserve}
                        onCancel={onCancel}
                        onEdit={onEdit}
                        onConfirmCancel={onConfirmCancel}
                      />
                    ))}
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
