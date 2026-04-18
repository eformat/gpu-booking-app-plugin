import * as React from 'react';
import { Helmet } from 'react-helmet';
import {
  PageSection,
  Title,
  Button,
  Alert,
  Spinner,
  Bullseye,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
  EmptyState,
  EmptyStateBody,
} from '@patternfly/react-core';
import { SyncIcon, UserIcon, OutlinedQuestionCircleIcon } from '@patternfly/react-icons';
import { Link } from 'react-router-dom';
import { useAuth } from '../utils/AuthContext';
import {
  getBookings,
  getConfig,
  createBooking as apiCreateBooking,
  createBulkBooking,
  cancelBooking,
} from '../utils/api';
import {
  FALLBACK_GPU_RESOURCES,
  GPUResource,
  Booking,
  todayStr,
  DEFAULT_BOOKING_WINDOW_DAYS,
} from '../utils/constants';
import ResourceSelector from './ResourceSelector';
import CalendarGrid from './CalendarGrid';
import GpuUsagePanel from './GpuUsagePanel';
import BookingGrid from './BookingGrid';
import BookingModal from './BookingModal';

const BookingPage: React.FC = () => {
  useAuth();
  const [bookings, setBookings] = React.useState<Booking[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [reservingKey, setReservingKey] = React.useState<string | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [gpuResources, setGpuResources] = React.useState<GPUResource[]>(FALLBACK_GPU_RESOURCES);
  const [selectedResources, setSelectedResources] = React.useState<string[]>([FALLBACK_GPU_RESOURCES[0].type]);
  const [confirmCancelId, setConfirmCancelId] = React.useState<string | null>(null);
  const [bookingWindowDays, setBookingWindowDays] = React.useState(DEFAULT_BOOKING_WINDOW_DAYS);
  const [utcNow, setUtcNow] = React.useState('');
  const [activeReservations, setActiveReservations] = React.useState<Record<string, string>>({});
  const [currentUser, setCurrentUser] = React.useState('');
  const [showBookingModal, setShowBookingModal] = React.useState(false);
  const [editBooking, setEditBooking] = React.useState<Booking | null>(null);
  const [contextMenu, setContextMenu] = React.useState<{ x: number; y: number } | null>(null);
  const [showingMyBookings, setShowingMyBookings] = React.useState(false);

  const now = new Date();
  const [viewYear, setViewYear] = React.useState(now.getFullYear());
  const [viewMonth, setViewMonth] = React.useState(now.getMonth());
  const [selectedDates, setSelectedDates] = React.useState<string[]>([todayStr()]);

  const selectedResourceObjects = gpuResources.filter((r) => selectedResources.includes(r.type));
  const gridDates = React.useMemo(() => [...selectedDates].sort(), [selectedDates]);

  const fetchBookings = React.useCallback(async () => {
    try {
      const result = await getBookings();
      setBookings(result.bookings || []);
      setActiveReservations(result.activeReservations || {});
      setCurrentUser(result.currentUser || '');
    } catch (e) {
      // ignore fetch errors silently
    }
    setLoading(false);
  }, []);

  React.useEffect(() => {
    getConfig()
      .then((data) => {
        if (data.bookingWindowDays) setBookingWindowDays(data.bookingWindowDays);
        if (Array.isArray(data.resources) && data.resources.length > 0) {
          setGpuResources(data.resources);
        }
      })
      .catch(() => {});
  }, []);

  React.useEffect(() => {
    fetchBookings();
    const interval = setInterval(fetchBookings, 30000);
    return () => clearInterval(interval);
  }, [fetchBookings]);

  React.useEffect(() => {
    function updateClock() {
      const n = new Date();
      setUtcNow(
        n.toLocaleString(undefined, {
          year: 'numeric', month: '2-digit', day: '2-digit',
          hour: '2-digit', minute: '2-digit', second: '2-digit',
          timeZoneName: 'short',
        }),
      );
    }
    updateClock();
    const interval = setInterval(updateClock, 1000);
    return () => clearInterval(interval);
  }, []);

  React.useEffect(() => {
    if (!contextMenu) return;
    const close = () => setContextMenu(null);
    window.addEventListener('click', close);
    return () => window.removeEventListener('click', close);
  }, [contextMenu]);

  // Navigation limits
  const maxDate = new Date();
  maxDate.setDate(maxDate.getDate() + bookingWindowDays);
  const earliestBooking = bookings.length > 0
    ? bookings.reduce((min, b) => (b.date < min ? b.date : min), bookings[0].date)
    : null;
  const earliestDate = earliestBooking
    ? (() => { const [ey, em] = earliestBooking.split('-').map(Number); return new Date(ey, em - 1, 1); })()
    : now;
  const canGoBack = viewYear > earliestDate.getFullYear() || (viewYear === earliestDate.getFullYear() && viewMonth > earliestDate.getMonth());
  const canGoForward = viewYear < maxDate.getFullYear() || (viewYear === maxDate.getFullYear() && viewMonth < maxDate.getMonth());

  const handleReserve = async (resource: string, slotIndex: number, date: string, slotType: string) => {
    const key = `${resource}-${slotIndex}-${date}-${slotType}`;
    setReservingKey(key);
    setError(null);
    try {
      await apiCreateBooking({ resource, slotIndex, date, slotType });
      await fetchBookings();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to reserve');
    }
    setReservingKey(null);
  };

  const handleCancel = async (id: string) => {
    setError(null);
    try {
      await cancelBooking(id);
      setConfirmCancelId(null);
      await fetchBookings();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to cancel');
    }
  };

  const handleBulkBooking = async (
    resources: Record<string, number>,
    startDate: string,
    endDate: string,
    description: string,
    startHourUtc: number,
    endHourUtc: number,
  ) => {
    if (editBooking) {
      await cancelBooking(editBooking.id);
    }
    await createBulkBooking({ resources, startDate, endDate, description, startHour: startHourUtc, endHour: endHourUtc });
    setShowBookingModal(false);
    setEditBooking(null);
    await fetchBookings();
  };

  const navigateMonth = (delta: number) => {
    let newMonth = viewMonth + delta;
    let newYear = viewYear;
    if (newMonth > 11) { newMonth = 0; newYear++; }
    else if (newMonth < 0) { newMonth = 11; newYear--; }
    setViewMonth(newMonth);
    setViewYear(newYear);
    setSelectedDates([]);
  };

  const handleShowMyBookings = () => {
    if (!currentUser) return;
    if (showingMyBookings) {
      setShowingMyBookings(false);
      setSelectedResources([gpuResources[0].type]);
      setViewMonth(now.getMonth());
      setViewYear(now.getFullYear());
      setSelectedDates([todayStr()]);
      return;
    }
    const myBookings = bookings.filter((b) => b.user === currentUser && b.source === 'reserved');
    const myDates = Array.from(new Set(myBookings.map((b) => b.date))).sort();
    if (myDates.length === 0) return;
    // Auto-select resource types that have user bookings
    const myResourceTypes = Array.from(new Set(myBookings.map((b) => b.resource)));
    setSelectedResources(myResourceTypes);
    // Navigate calendar to the month of the earliest booking
    const [fy, fm] = myDates[0].split('-').map(Number);
    setViewYear(fy);
    setViewMonth(fm - 1);
    setSelectedDates(myDates);
    setShowingMyBookings(true);
  };

  const handleContextMenu = (dateStr: string, e: React.MouseEvent) => {
    e.preventDefault();
    if (!selectedDates.includes(dateStr)) {
      setSelectedDates([dateStr]);
    }
    setContextMenu({ x: e.clientX, y: e.clientY });
  };

  if (loading) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }

  return (
    <>
      <Helmet>
        <title>GPU Booking</title>
      </Helmet>
      <>
        <PageSection style={{ paddingBottom: '24px' }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' }}>
            <div>
              <Title headingLevel="h1" size="2xl">
                GPU Resource Booking
              </Title>
              <div style={{ color: 'var(--pf-t--global--text--color--regular)', opacity: 0.7, marginTop: '8px' }}>
                Reserve H200 GPU resources with MIG partitioning
              </div>
              <div style={{ fontFamily: 'monospace', color: 'var(--pf-t--global--text--color--regular)', opacity: 0.7, fontSize: '13px', marginTop: '4px' }}>
                {utcNow}
              </div>
            </div>
            <Toolbar>
              <ToolbarContent>
                {currentUser && (
                  <ToolbarItem>
                    <Button
                      variant={showingMyBookings ? 'primary' : 'secondary'}
                      onClick={handleShowMyBookings}
                      icon={<UserIcon />}
                    >
                      My Bookings
                    </Button>
                  </ToolbarItem>
                )}
                <ToolbarItem>
                  <Button
                    variant="secondary"
                    onClick={() => { setLoading(true); fetchBookings(); }}
                    icon={<SyncIcon />}
                  >
                    Refresh
                  </Button>
                </ToolbarItem>
                <ToolbarItem>
                  <Link to="/gpu-booking/help/getting-started" style={{ textDecoration: 'none' }}>
                    <Button
                      variant="secondary"
                      icon={<OutlinedQuestionCircleIcon />}
                    >
                      Help
                    </Button>
                  </Link>
                </ToolbarItem>
              </ToolbarContent>
            </Toolbar>
          </div>

          <div style={{ marginTop: '24px' }}>
            <ResourceSelector
              resources={gpuResources}
              selectedResources={selectedResources}
              onSelectionChange={setSelectedResources}
            />
          </div>
        </PageSection>

        <PageSection>
          {error && (
            <Alert
              variant="danger"
              title={error}
              isInline
              actionClose={<Button variant="plain" onClick={() => setError(null)}>&times;</Button>}
              style={{ marginBottom: '16px' }}
            />
          )}

          <GpuUsagePanel
            bookings={bookings}
            resources={gpuResources}
            selectedDate={selectedDates[0] || todayStr()}
          />

          <CalendarGrid
            viewYear={viewYear}
            viewMonth={viewMonth}
            selectedDates={selectedDates}
            bookings={bookings}
            bookingWindowDays={bookingWindowDays}
            gpuResources={gpuResources}
            canGoBack={canGoBack}
            canGoForward={canGoForward}
            onNavigateMonth={navigateMonth}
            onSelectDates={(dates) => { setSelectedDates(dates); setShowingMyBookings(false); }}
            onContextMenu={handleContextMenu}
            onGoToToday={() => {
              setViewMonth(now.getMonth());
              setViewYear(now.getFullYear());
              setSelectedDates([todayStr()]);
            }}
          />

          <div style={{ margin: '16px 0', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
            <div style={{ fontSize: '14px', color: 'var(--pf-t--global--text--color--regular)' }}>
              {selectedDates.length === 0
                ? 'Click a date to view'
                : `${selectedDates.length} date${selectedDates.length > 1 ? 's' : ''} selected`}
              <span style={{ marginLeft: '4px', opacity: 0.7 }}>(Ctrl+click to multi-select, Shift+click for range)</span>
            </div>
          </div>

          {gridDates.length === 0 ? (
            <EmptyState>
              <EmptyStateBody>
                Click a date in the calendar above to view bookings.
              </EmptyStateBody>
            </EmptyState>
          ) : (
            selectedResourceObjects.map((resource) => (
              <BookingGrid
                key={resource.type}
                resource={resource}
                dates={gridDates}
                bookings={bookings}
                activeReservations={activeReservations}
                currentUser={currentUser}
                reservingKey={reservingKey}
                confirmCancelId={confirmCancelId}
                onReserve={handleReserve}
                onCancel={handleCancel}
                onEdit={(b) => { setEditBooking(b); setShowBookingModal(true); }}
                onConfirmCancel={setConfirmCancelId}
              />
            ))
          )}
        </PageSection>

        {/* Context menu */}
        {contextMenu && (
          <div
            style={{
              position: 'fixed',
              left: contextMenu.x,
              top: contextMenu.y,
              zIndex: 1000,
              backgroundColor: 'white',
              borderRadius: '8px',
              boxShadow: '0 4px 16px rgba(0,0,0,0.15)',
              border: '1px solid var(--pf-t--global--border--color--default)',
              padding: '4px 0',
              minWidth: '160px',
            }}
          >
            <button
              onClick={() => {
                setContextMenu(null);
                setShowBookingModal(true);
              }}
              style={{
                width: '100%',
                textAlign: 'left',
                padding: '8px 16px',
                border: 'none',
                background: 'none',
                cursor: 'pointer',
                fontSize: '14px',
              }}
            >
              Book GPU
            </button>
          </div>
        )}

        {/* Booking modal */}
        {(showBookingModal && (selectedDates.length > 0 || editBooking)) && (
          <BookingModal
            isOpen={showBookingModal}
            startDate={editBooking?.date || [...selectedDates].sort()[0]}
            endDate={editBooking?.date || [...selectedDates].sort()[selectedDates.length - 1]}
            bookings={bookings}
            editBooking={editBooking || undefined}
            gpuResources={gpuResources}
            onClose={() => { setShowBookingModal(false); setEditBooking(null); }}
            onSubmit={handleBulkBooking}
          />
        )}
      </>
    </>
  );
};

export default BookingPage;
