import * as React from 'react';
import { Booking, GPUResource, FALLBACK_GPU_RESOURCES, DEFAULT_BOOKING_WINDOW_DAYS } from './constants';
import { getBookings, getConfig } from './api';

export interface BookingsState {
  bookings: Booking[];
  activeReservations: Record<string, string>;
  currentUser: string;
  loading: boolean;
  fetchBookings: () => Promise<void>;
}

export function useBookings(): BookingsState {
  const [bookings, setBookings] = React.useState<Booking[]>([]);
  const [activeReservations, setActiveReservations] = React.useState<Record<string, string>>({});
  const [currentUser, setCurrentUser] = React.useState('');
  const [loading, setLoading] = React.useState(true);

  const fetchBookings = React.useCallback(async () => {
    try {
      const result = await getBookings();
      setBookings(result.bookings || []);
      setActiveReservations(result.activeReservations || {});
      setCurrentUser(result.currentUser || '');
    } catch (e) {
      console.warn('Failed to fetch bookings:', e);
    }
    setLoading(false);
  }, []);

  React.useEffect(() => {
    fetchBookings();
    const interval = setInterval(fetchBookings, 30000);
    return () => clearInterval(interval);
  }, [fetchBookings]);

  return { bookings, activeReservations, currentUser, loading, fetchBookings };
}

export interface ConfigState {
  gpuResources: GPUResource[];
  bookingWindowDays: number;
}

export function useConfig(): ConfigState {
  const [gpuResources, setGpuResources] = React.useState<GPUResource[]>(FALLBACK_GPU_RESOURCES);
  const [bookingWindowDays, setBookingWindowDays] = React.useState(DEFAULT_BOOKING_WINDOW_DAYS);

  React.useEffect(() => {
    getConfig()
      .then((data) => {
        if (data.bookingWindowDays) setBookingWindowDays(data.bookingWindowDays);
        if (Array.isArray(data.resources) && data.resources.length > 0) {
          setGpuResources(data.resources);
        }
      })
      .catch((e) => {
        console.warn('Failed to fetch config:', e);
      });
  }, []);

  return { gpuResources, bookingWindowDays };
}

export function useClock(): string {
  const [utcNow, setUtcNow] = React.useState('');

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

  return utcNow;
}
