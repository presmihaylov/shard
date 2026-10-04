// Read the daemon's JSON records field by field, and refuse a whole record at the first field the wire does not allow.
import { ProtocolError, isObject } from "./errors.js";

export function isStrings(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((item) => typeof item === "string");
}

export function date(value: unknown): Date | undefined {
  const parsed = typeof value === "string" ? new Date(value) : undefined;

  return parsed && !Number.isNaN(parsed.getTime()) ? parsed : undefined;
}

/** Fields reads one record; a key the daemon leaves out under omitempty reads as its optional form. */
export class Fields {
  private constructor(
    private readonly record: Record<string, unknown>,
    private readonly what: string,
  ) {}

  static of(value: unknown, what: string): Fields {
    if (!isObject(value)) {
      throw new ProtocolError(`the daemon answered ${JSON.stringify(value)} as ${what}`);
    }

    return new Fields(value, what);
  }

  string(key: string): string {
    const value = this.record[key];
    if (typeof value !== "string") {
      throw this.refuse(key);
    }

    return value;
  }

  optionalString(key: string): string | null {
    return this.absent(key) ? null : this.string(key);
  }

  int(key: string): number {
    const value = this.record[key];
    if (typeof value !== "number" || !Number.isInteger(value)) {
      throw this.refuse(key);
    }

    return value;
  }

  optionalInt(key: string, fallback: number): number {
    return this.absent(key) ? fallback : this.int(key);
  }

  bool(key: string): boolean {
    const value = this.record[key] ?? false;
    if (typeof value !== "boolean") {
      throw this.refuse(key);
    }

    return value;
  }

  strings(key: string): string[] {
    const value = this.record[key] ?? [];
    if (!isStrings(value)) {
      throw this.refuse(key);
    }

    return value;
  }

  ints(key: string): number[] {
    const value = this.record[key] ?? [];
    if (!Array.isArray(value) || !value.every((item) => typeof item === "number" && Number.isInteger(item))) {
      throw this.refuse(key);
    }

    return value;
  }

  date(key: string): Date {
    const value = date(this.record[key]);
    if (!value) {
      throw this.refuse(key);
    }

    return value;
  }

  optionalDate(key: string): Date | null {
    return this.absent(key) ? null : this.date(key);
  }

  object(key: string): Fields {
    const value = this.record[key];
    if (!isObject(value)) {
      throw this.refuse(key);
    }

    return new Fields(value, `${key} of ${this.what}`);
  }

  optionalObject(key: string): Fields | null {
    return this.absent(key) ? null : this.object(key);
  }

  list(key: string): Fields[] {
    const value = this.record[key] ?? [];
    if (!Array.isArray(value)) {
      throw this.refuse(key);
    }

    return value.map((item) => Fields.of(item, `an entry of ${key} of ${this.what}`));
  }

  /** oneOf reads a string the wire allows only some values of. */
  oneOf<T extends string>(key: string, allowed: readonly T[]): T {
    const value = this.string(key);
    const found = allowed.find((each) => each === value);
    if (found === undefined) {
      throw this.refuse(key);
    }

    return found;
  }

  private absent(key: string): boolean {
    return this.record[key] === undefined || this.record[key] === null;
  }

  private refuse(key: string): ProtocolError {
    return new ProtocolError(`the daemon answered ${this.what} whose ${key} is ${JSON.stringify(this.record[key])}`);
  }
}
