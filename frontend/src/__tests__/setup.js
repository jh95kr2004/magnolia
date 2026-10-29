import '@testing-library/jest-dom';

// Mock EventSource globally for SSE unit tests with event triggering support
export class MockEventSource {
  static instances = [];

  constructor(url, options) {
    this.url = url;
    this.options = options;
    this.listeners = {};
    this.closed = false;
    MockEventSource.instances.push(this);
  }

  addEventListener(event, callback) {
    if (!this.listeners[event]) {
      this.listeners[event] = [];
    }
    this.listeners[event].push(callback);
  }

  emit(event, payloadObj) {
    const callbacks = this.listeners[event] || [];
    const evt = { data: typeof payloadObj === 'string' ? payloadObj : JSON.stringify(payloadObj) };
    callbacks.forEach((cb) => cb(evt));
  }

  close() {
    this.closed = true;
  }

  static reset() {
    MockEventSource.instances = [];
  }
}

globalThis.EventSource = MockEventSource;
