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

it('lands only the installed factory\'s work: not a delete an earlier factory left blocked', async () => {
  const request = indexedDB.open('held', 1);
  await landIndexedDB();
  // The open connection blocks the delete; neither is ever closed here.
  const blocked = indexedDB.deleteDatabase('held');
  const outcome = new Promise((resolve) => {
    blocked.onblocked = () => resolve('blocked');
    blocked.onsuccess = () => resolve('deleted');
  });
  expect(await outcome).toBe('blocked');
  installFakeIndexedDB();
  await landIndexedDB();
  request.result.close();
});
