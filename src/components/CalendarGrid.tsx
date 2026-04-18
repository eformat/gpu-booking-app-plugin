import * as React from 'react';
import { Button, Title } from '@patternfly/react-core';
import { AngleLeftIcon, AngleRightIcon } from '@patternfly/react-icons';
import {
  DAY_HEADERS,
  MONTH_NAMES,
  GPUResource,
  Booking,
  buildGpuEquivalentMap,
  totalGpuEquivalents,
  getMonthDates,
  getMonthStartOffset,
  isInBookingWindow,
  isPastDate,
  isWeekend,
  todayStr,
  getDateRange,
} from '../utils/constants';
import './styles.css';

interface CalendarGridProps {
  viewYear: number;
  viewMonth: number;
  selectedDates: string[];
  bookings: Booking[];
  bookingWindowDays: number;
  gpuResources: GPUResource[];
  canGoBack: boolean;
  canGoForward: boolean;
  onNavigateMonth: (delta: number) => void;
  onSelectDates: (dates: string[]) => void;
  onContextMenu: (dateStr: string, e: React.MouseEvent) => void;
  onGoToToday: () => void;
}

const CalendarGrid: React.FC<CalendarGridProps> = ({
  viewYear,
  viewMonth,
  selectedDates,
  bookings,
  bookingWindowDays,
  gpuResources,
  canGoBack,
  canGoForward,
  onNavigateMonth,
  onSelectDates,
  onContextMenu,
  onGoToToday,
}) => {
  const lastClickedDate = React.useRef<string | null>(null);
  const now = new Date();
  const monthDates = getMonthDates(viewYear, viewMonth);
  const monthStartOffset = getMonthStartOffset(viewYear, viewMonth);

  const gpuEquivMap = React.useMemo(() => buildGpuEquivalentMap(gpuResources), [gpuResources]);
  const totalEquiv = React.useMemo(() => totalGpuEquivalents(gpuResources), [gpuResources]);

  const getDateGpuUsage = (date: string): number => {
    let total = 0;
    for (const r of gpuResources) {
      const units = new Set(
        bookings.filter((b) => b.resource === r.type && b.date === date).map((b) => b.slotIndex),
      );
      total += units.size * (gpuEquivMap[r.type] || 0);
    }
    return total;
  };

  const handleClick = (dateStr: string, e: React.MouseEvent) => {
    if (e.shiftKey && lastClickedDate.current) {
      const range = getDateRange(lastClickedDate.current, dateStr);
      if (e.ctrlKey || e.metaKey) {
        onSelectDates(Array.from(new Set([...selectedDates, ...range])));
      } else {
        onSelectDates(range);
      }
    } else if (e.ctrlKey || e.metaKey) {
      if (selectedDates.includes(dateStr)) {
        onSelectDates(selectedDates.filter((d) => d !== dateStr));
      } else {
        onSelectDates([...selectedDates, dateStr]);
      }
    } else {
      onSelectDates([dateStr]);
    }
    lastClickedDate.current = dateStr;
  };

  const showTodayButton = viewMonth !== now.getMonth() || viewYear !== now.getFullYear();

  return (
    <div className="gpu-calendar-container">
      <div className="gpu-calendar-header">
        <Button
          variant="plain"
          isDisabled={!canGoBack}
          onClick={() => onNavigateMonth(-1)}
          aria-label="Previous month"
        >
          <AngleLeftIcon />
        </Button>
        <div style={{ textAlign: 'center' }}>
          <Title headingLevel="h2" size="xl">
            {MONTH_NAMES[viewMonth]} {viewYear}
          </Title>
          <div style={{ fontSize: '12px', color: 'var(--pf-t--global--text--color--regular)', opacity: 0.7, marginTop: '2px' }}>
            Booking window: {bookingWindowDays} days from today
          </div>
        </div>
        <Button
          variant="plain"
          isDisabled={!canGoForward}
          onClick={() => onNavigateMonth(1)}
          aria-label="Next month"
        >
          <AngleRightIcon />
        </Button>
      </div>

      <div className="gpu-calendar-grid">
        {DAY_HEADERS.map((d) => (
          <div key={d} className="gpu-calendar-day-header">
            {d}
          </div>
        ))}

        {Array.from({ length: monthStartOffset }, (_, i) => (
          <div key={`empty-${i}`} className="gpu-calendar-cell gpu-calendar-cell--empty" />
        ))}

        {monthDates.map((date) => {
          const dayNum = parseInt(date.split('-')[2], 10);
          const today = date === todayStr();
          const weekend = isWeekend(date);
          const bookable = isInBookingWindow(date, bookingWindowDays);
          const gpuUsage = getDateGpuUsage(date);
          const hasBookings = bookings.some((b) => b.date === date);
          const past = isPastDate(date);
          const clickable = bookable || (past && hasBookings);
          const isSelected = selectedDates.includes(date);

          let cellClass = 'gpu-calendar-cell';
          if (today) cellClass += ' gpu-calendar-cell--today';
          if (isSelected && !today) cellClass += ' gpu-calendar-cell--selected';
          else if (!today && !clickable) cellClass += ' gpu-calendar-cell--disabled';
          else if (!today && past && hasBookings) cellClass += ' gpu-calendar-cell--past';
          else if (!today && weekend) cellClass += ' gpu-calendar-cell--weekend';

          return (
            <button
              key={date}
              className={cellClass}
              onClick={(e) => clickable && handleClick(date, e)}
              onContextMenu={(e) => {
                if (clickable && !past) onContextMenu(date, e);
              }}
              disabled={!clickable}
            >
              {today && <span className="gpu-calendar-today-badge">TODAY</span>}
              <span className={`gpu-calendar-day-number${today ? ' gpu-calendar-day-number--today' : isSelected ? ' gpu-calendar-day-number--highlight' : ''}`}>
                {dayNum}
              </span>
              {(bookable || (past && hasBookings)) && gpuUsage > 0 && (
                <span
                  className={`gpu-calendar-usage-badge${gpuUsage >= totalEquiv ? ' gpu-calendar-usage-badge--full' : ''}`}
                >
                  {gpuUsage % 1 === 0 ? gpuUsage : gpuUsage.toFixed(1)}/{totalEquiv}
                </span>
              )}
            </button>
          );
        })}
      </div>

      {showTodayButton && (
        <div style={{ textAlign: 'center', marginTop: '12px' }}>
          <Button variant="link" onClick={onGoToToday}>
            Back to today
          </Button>
        </div>
      )}
    </div>
  );
};

export default CalendarGrid;
