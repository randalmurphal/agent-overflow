import { it, expect } from 'vitest';
import { page } from 'vitest/browser';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import '../../../app.css';
import DeviceNameField from './DeviceNameField.svelte';
import { clientDeviceName } from '../../stores/clientDeviceName.svelte';

it.each([360, 1280])('keeps the editable device name and save control usable at %ipx', async (width) => {
  await page.viewport(width, 800);
  localStorage.setItem('agent-overflow:deviceSession', JSON.stringify({ sessionId: 'phone', credential: 'test', expiresAtMs: Date.now() + 60000 }));
  const view = render(DeviceNameField);
  const input = view.getByLabelText('Device name');
  const save = view.getByRole('button', { name: 'Save' });
  await fireEvent.input(input, { target: { value: 'Randy’s Pixel — development phone' } });
  await waitFor(() => {
    for (const element of [input, save]) {
      const rect = element.getBoundingClientRect();
      expect(rect.width).toBeGreaterThan(0);
      expect(rect.left).toBeGreaterThanOrEqual(0);
      expect(rect.right).toBeLessThanOrEqual(width);
    }
  });
  await fireEvent.click(save);
  expect(clientDeviceName()).toBe('Randy’s Pixel — development phone');
  expect(view.getByRole('status')).toHaveTextContent('Device name saved.');
  localStorage.removeItem('agent-overflow:deviceSession');
});
