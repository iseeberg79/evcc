import { describe, expect, it, vi } from "vitest";
import { createServiceEndpoints, fetchServiceValues, type TemplateParam, type DeviceValues } from "./index";

const buildParam = (name: string, service?: string, serviceDependencies?: string[][]): TemplateParam => ({
  Name: name,
  Required: false,
  Advanced: false,
  Deprecated: false,
  Service: service,
  ServiceDependencies: serviceDependencies,
});

describe("createServiceEndpoints", () => {
  it("skips params without service", () => {
    const params = [buildParam("home", "homes"), buildParam("power", "homes/{home}/sensors")];
    const endpoints = createServiceEndpoints(params);
    expect(endpoints.map((endpoint) => endpoint.name)).toEqual(["home", "power"]);
  });

  it("replaces single placeholder", () => {
    const params = [buildParam("home", "homes"), buildParam("power", "homes/{home}/sensors")];
    const endpoints = createServiceEndpoints(params);
    const homeEndpoint = endpoints.find(({ name }) => name === "home")!;
    const powerEndpoint = endpoints.find(({ name }) => name === "power")!;
    expect(homeEndpoint.dependencies).toEqual([]);
    expect(homeEndpoint.url({})).toBe("homes");
    expect(powerEndpoint.dependencies).toEqual(["home"]);
    expect(powerEndpoint.url({ home: "main" })).toBe("homes/main/sensors");
    expect(powerEndpoint.url({ home: "with space" })).toBe("homes/with%20space/sensors");
    expect(powerEndpoint.url({} as Record<string, string>)).toBe("homes/{home}/sensors");
  });

  it("replaces multiple placeholders", () => {
    const params = [
      buildParam("home", "homes"),
      buildParam("sensor", "homes/{home}/sensors/{sensor}"),
    ];
    const endpoints = createServiceEndpoints(params);
    const sensorEndpoint = endpoints.find(({ name }) => name === "sensor")!;
    expect(sensorEndpoint.dependencies).toEqual(["home", "sensor"]);
    expect(sensorEndpoint.url({ home: "hq", sensor: "battery" })).toBe("homes/hq/sensors/battery");
  });

  it("encodes replacements", () => {
    const params = [buildParam("token", "homes/{home}/sensors/{sensor}?token={token}")];
    const endpoints = createServiceEndpoints(params);
    const tokenEndpoint = endpoints[0]!;
    expect(tokenEndpoint.url({ home: "hq", sensor: "bat/tery", token: "a+b c" })).toBe(
      "homes/hq/sensors/bat%2Ftery?token=a%2Bb%20c"
    );
    expect(tokenEndpoint.url({} as Record<string, string>)).toBe(
      "homes/{home}/sensors/{sensor}?token={token}"
    );
  });

  it("extracts dependency groups", () => {
    const params = [
      buildParam("capacity", "modbus/params?uri={host}:{port}&device={device}&id={id}&address=1068", [
        ["host", "port", "id"],
        ["device", "id"],
      ]),
    ];
    const endpoints = createServiceEndpoints(params);
    expect(endpoints[0]!.dependencyGroups).toEqual([
      ["host", "port", "id"],
      ["device", "id"],
    ]);
  });

  it("builds correct URLs with partial placeholders (TCP/IP mode)", () => {
    const params = [
      buildParam("capacity", "modbus/params?uri={host}:{port}&device={device}&baudrate={baudrate}&id={id}&address=1068", [
        ["host", "port", "id"],
        ["device", "baudrate", "id"],
      ]),
    ];
    const endpoints = createServiceEndpoints(params);
    const capacity = endpoints.find((e) => e.name === "capacity")!;

    // TCP/IP only - device and baudrate remain as {placeholder}
    const url = capacity.url({
      host: "192.168.1.1",
      port: "502",
      id: "1",
    });

    expect(url).toContain("uri=192.168.1.1:502");
    expect(url).toContain("{device}");
    expect(url).toContain("{baudrate}");
  });

  it("preserves falsy values like id=0 in URL parameters", () => {
    const params = [buildParam("capacity", "modbus/params?id={id}&address=1068")];
    const endpoints = createServiceEndpoints(params);
    const capacity = endpoints.find((e) => e.name === "capacity")!;

    const url = capacity.url({ id: 0 });
    expect(url).toContain("id=0");
  });
});

describe("fetchServiceValues with dependency groups", () => {
  it("calls service when first dependency group is satisfied (TCP/IP)", async () => {
    const mockLoader = vi.fn().mockResolvedValue(["5000"]);

    const params = [
      buildParam("capacity", "modbus/params?uri={host}:{port}&device={device}&id={id}&address=1068", [
        ["host", "port", "id"],
        ["device", "id"],
      ]),
    ];

    const values: DeviceValues = {
      type: "meter",
      template: "growatt",
      host: "192.168.1.1",
      port: "502",
      id: "1",
    };

    const result = await fetchServiceValues(params, values, mockLoader);

    expect(result["capacity"]).toEqual(["5000"]);
    expect(mockLoader).toHaveBeenCalledTimes(1);

    // Verify URL cleanup worked - no {device} in URL
    const callArg = mockLoader.mock.calls[0]![0];
    expect(callArg).not.toContain("{device}");
    expect(callArg).toContain("uri=192.168.1.1:502");
  });

  it("calls service when second dependency group is satisfied (RS485)", async () => {
    const mockLoader = vi.fn().mockResolvedValue(["5000"]);

    const params = [
      buildParam("capacity", "modbus/params?uri={host}:{port}&device={device}&baudrate={baudrate}&id={id}&address=1068", [
        ["host", "port", "id"],
        ["device", "baudrate", "id"],
      ]),
    ];

    const values: DeviceValues = {
      type: "meter",
      template: "growatt",
      device: "/dev/ttyUSB0",
      baudrate: "9600",
      id: "1",
    };

    const result = await fetchServiceValues(params, values, mockLoader);

    expect(result["capacity"]).toEqual(["5000"]);

    // Verify URL cleanup worked - no {host} or {port} in URL
    const callArg = mockLoader.mock.calls[0]![0];
    expect(callArg).not.toContain("{host}");
    expect(callArg).toContain("device=%2Fdev%2FttyUSB0");
  });

  it("skips service call when no dependency group is satisfied", async () => {
    const mockLoader = vi.fn().mockResolvedValue(["5000"]);

    const params = [
      buildParam("capacity", "modbus/params?uri={host}:{port}&device={device}&id={id}&address=1068", [
        ["host", "port", "id"],
        ["device", "id"],
      ]),
    ];

    const values: DeviceValues = {
      type: "meter",
      template: "growatt",
      // Missing all required params
    };

    const result = await fetchServiceValues(params, values, mockLoader);

    expect(result["capacity"]).toBeUndefined();
    expect(mockLoader).not.toHaveBeenCalled();
  });

  it("handles falsy values like id=0 correctly", async () => {
    const mockLoader = vi.fn().mockResolvedValue(["5000"]);

    const params = [
      buildParam("capacity", "modbus/params?uri={host}:{port}&id={id}&address=1068", [
        ["host", "port", "id"],
      ]),
    ];

    const values: DeviceValues = {
      type: "meter",
      template: "growatt",
      host: "192.168.1.1",
      port: "502",
      id: 0,  // Falsy but valid
    };

    const result = await fetchServiceValues(params, values, mockLoader);

    expect(result["capacity"]).toEqual(["5000"]);
    const callArg = mockLoader.mock.calls[0]![0];
    expect(callArg).toContain("id=0");
  });

  it("falls back to single dependency logic when no groups defined", async () => {
    const mockLoader = vi.fn().mockResolvedValue(["5000"]);

    const params = [buildParam("power", "homes/{home}/sensors")];

    const values: DeviceValues = {
      type: "meter",
      template: "test",
      home: "main",
    };

    const result = await fetchServiceValues(params, values, mockLoader);
    expect(result["power"]).toEqual(["5000"]);
  });
});
