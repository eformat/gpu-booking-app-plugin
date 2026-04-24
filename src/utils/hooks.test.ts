// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react-hooks';

vi.mock('./api', () => ({
  getBookings: vi.fn(),
  getConfig: vi.fn(),
  getPreemptedWorkloads: vi.fn(),
}));

import { getBookings, getConfig, getPreemptedWorkloads } from './api';
import { useBookings, useConfig, usePreemptedWorkloads, useClock } from './hooks';

const mockGetBookings = vi.mocked(getBookings);
const mockGetConfig = vi.mocked(getConfig);
const mockGetPreemptedWorkloads = vi.mocked(getPreemptedWorkloads);

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('useBookings', () => {
  it('fetches bookings on mount and returns data', async () => {
    mockGetBookings.mockResolvedValue({
      bookings: [{ id: 'booking-1', user: 'alice', resource: 'nvidia.com/gpu' } as any],
      activeReservations: { alice: 'user-alice' },
      currentUser: 'alice',
    });

    const { result, waitForNextUpdate } = renderHook(() => useBookings());

    expect(result.current.loading).toBe(true);

    await waitForNextUpdate();

    expect(result.current.loading).toBe(false);
    expect(result.current.bookings).toHaveLength(1);
    expect(result.current.bookings[0].id).toBe('booking-1');
    expect(result.current.currentUser).toBe('alice');
    expect(result.current.activeReservations).toEqual({ alice: 'user-alice' });
  });

  it('handles fetch failure gracefully', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    mockGetBookings.mockRejectedValue(new Error('network error'));

    const { result, waitForNextUpdate } = renderHook(() => useBookings());

    await waitForNextUpdate();

    expect(result.current.loading).toBe(false);
    expect(result.current.bookings).toEqual([]);
    expect(result.current.currentUser).toBe('');
  });

  it('polls every 30 seconds', async () => {
    mockGetBookings.mockResolvedValue({
      bookings: [],
      activeReservations: {},
      currentUser: 'alice',
    });

    const { waitForNextUpdate } = renderHook(() => useBookings());

    await waitForNextUpdate();
    expect(mockGetBookings).toHaveBeenCalledTimes(1);

    act(() => {
      vi.advanceTimersByTime(30000);
    });

    await waitForNextUpdate();
    expect(mockGetBookings).toHaveBeenCalledTimes(2);
  });

  it('cleans up interval on unmount', async () => {
    mockGetBookings.mockResolvedValue({
      bookings: [],
      activeReservations: {},
      currentUser: '',
    });

    const { unmount, waitForNextUpdate } = renderHook(() => useBookings());

    await waitForNextUpdate();
    expect(mockGetBookings).toHaveBeenCalledTimes(1);

    unmount();

    act(() => {
      vi.advanceTimersByTime(60000);
    });

    expect(mockGetBookings).toHaveBeenCalledTimes(1);
  });

  it('handles null fields in response', async () => {
    mockGetBookings.mockResolvedValue({
      bookings: null as any,
      activeReservations: null as any,
      currentUser: null as any,
    });

    const { result, waitForNextUpdate } = renderHook(() => useBookings());

    await waitForNextUpdate();

    expect(result.current.bookings).toEqual([]);
    expect(result.current.activeReservations).toEqual({});
    expect(result.current.currentUser).toBe('');
  });
});

describe('useConfig', () => {
  it('returns fallback defaults initially', () => {
    mockGetConfig.mockReturnValue(new Promise(() => {}));

    const { result } = renderHook(() => useConfig());

    expect(result.current.bookingWindowDays).toBe(30);
    expect(result.current.gpuResources.length).toBeGreaterThan(0);
  });

  it('fetches and applies config', async () => {
    mockGetConfig.mockResolvedValue({
      resources: [{ name: 'Test GPU', type: 'test/gpu', count: 4, share: 0.1, gpuEquivalent: 2.0 }],
      bookingWindowDays: 60,
      totalCpu: 128,
      totalMemory: 2048,
    });

    const { result, waitForNextUpdate } = renderHook(() => useConfig());

    await waitForNextUpdate();

    expect(result.current.bookingWindowDays).toBe(60);
    expect(result.current.gpuResources).toHaveLength(1);
    expect(result.current.gpuResources[0].name).toBe('Test GPU');
  });

  it('keeps fallbacks on fetch failure', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    mockGetConfig.mockRejectedValue(new Error('network error'));

    const { result } = renderHook(() => useConfig());

    // No state update on failure — flush the rejected promise
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(result.current.bookingWindowDays).toBe(30);
    expect(result.current.gpuResources.length).toBeGreaterThan(0);
    expect(mockGetConfig).toHaveBeenCalledTimes(1);
  });

  it('keeps fallback resources if server returns empty array', async () => {
    mockGetConfig.mockResolvedValue({
      resources: [],
      bookingWindowDays: 14,
      totalCpu: 64,
      totalMemory: 1024,
    });

    const { result, waitForNextUpdate } = renderHook(() => useConfig());

    await waitForNextUpdate();

    expect(result.current.bookingWindowDays).toBe(14);
    expect(result.current.gpuResources.length).toBeGreaterThan(0);
  });
});

describe('usePreemptedWorkloads', () => {
  it('fetches preempted workloads on mount', async () => {
    mockGetPreemptedWorkloads.mockResolvedValue({
      workloads: [{ name: 'wl-1', namespace: 'ns', owner: 'alice', reason: 'preempted', message: '', timestamp: '' }],
    });

    const { result, waitForNextUpdate } = renderHook(() => usePreemptedWorkloads());

    await waitForNextUpdate();

    expect(result.current.preemptedWorkloads).toHaveLength(1);
    expect(result.current.preemptedWorkloads[0].name).toBe('wl-1');
  });

  it('handles failure silently', async () => {
    mockGetPreemptedWorkloads.mockRejectedValue(new Error('no kueue'));

    const { result } = renderHook(() => usePreemptedWorkloads());

    // Give the promise time to reject
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(result.current.preemptedWorkloads).toEqual([]);
  });

  it('polls every 30 seconds', async () => {
    mockGetPreemptedWorkloads.mockResolvedValue({ workloads: [] });

    const { waitForNextUpdate } = renderHook(() => usePreemptedWorkloads());

    await waitForNextUpdate();
    expect(mockGetPreemptedWorkloads).toHaveBeenCalledTimes(1);

    act(() => {
      vi.advanceTimersByTime(30000);
    });

    await waitForNextUpdate();
    expect(mockGetPreemptedWorkloads).toHaveBeenCalledTimes(2);
  });
});

describe('useClock', () => {
  it('returns a formatted time string', () => {
    const { result } = renderHook(() => useClock());

    // useClock calls updateClock() synchronously in useEffect, then sets state
    // With fake timers, flush the effect
    act(() => {
      vi.advanceTimersByTime(0);
    });

    expect(result.current.length).toBeGreaterThan(0);
  });

  it('updates every second', () => {
    const { result } = renderHook(() => useClock());

    act(() => {
      vi.advanceTimersByTime(1000);
    });

    expect(typeof result.current).toBe('string');
    expect(result.current.length).toBeGreaterThan(0);
  });

  it('cleans up interval on unmount', () => {
    const { unmount } = renderHook(() => useClock());

    act(() => {
      vi.advanceTimersByTime(0);
    });

    unmount();

    // Should not throw after unmount
    act(() => {
      vi.advanceTimersByTime(5000);
    });
  });
});
