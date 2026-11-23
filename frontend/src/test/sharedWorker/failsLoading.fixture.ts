// Fails while loading, after adding a listener: no test and no afterAll run.
import { listen } from './fixtureState';

listen('failsLoading');
throw new Error('fixture fails while loading');
