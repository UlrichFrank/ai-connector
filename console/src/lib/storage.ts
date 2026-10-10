/** localStorage that never throws (private windows, blocked storage): per-browser conveniences only. */
export const local = {
  get<T>(key: string, fallback: T): T {
    try {
      const v = localStorage.getItem("mlxlab." + key);
      return v == null ? fallback : (JSON.parse(v) as T);
    } catch {
      return fallback;
    }
  },
  set(key: string, value: unknown) {
    try {
      localStorage.setItem("mlxlab." + key, JSON.stringify(value));
    } catch {
      // not kept: fine
    }
  },
};
