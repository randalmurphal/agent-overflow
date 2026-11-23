import { expect, it } from 'vitest';
import { installFakeIndexedDB, landIndexedDB } from './helpers/fakeIndexedDB';

installFakeIndexedDB();

it('lands a chain of IndexedDB work however many steps it takes', async () => {
  const request = indexedDB.open('land', 1);
  request.onupgradeneeded = () => request.result.createObjectStore('rows');
  await landIndexedDB();
  const db = request.result;
  // 600 puts, each issued from the last one's success: one step each.
  let written = 0;
  const store = db.transaction('rows', 'readwrite').objectStore('rows');
  const next = () => {
    if (written === 600) return;
    store.put(written, written).onsuccess = () => {
      written += 1;
      next();
    };
  };
  next();
  await landIndexedDB();
  expect(written).toBe(600);
  db.close();
});
