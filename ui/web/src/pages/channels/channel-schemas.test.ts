import { describe, it, expect } from "vitest";
import { configSchema } from "./channel-schemas";

describe("pancake configSchema", () => {
  const pancakeConfig = configSchema["pancake"]!;

  it("has a platform field", () => {
    expect(pancakeConfig).toBeDefined();
    const platformField = pancakeConfig.find((f) => f.key === "platform");
    expect(platformField).toBeDefined();
  });

  it("platform field is type select", () => {
    const platformField = pancakeConfig.find((f) => f.key === "platform")!;
    expect(platformField.type).toBe("select");
  });

  it("platform field is required", () => {
    const platformField = pancakeConfig.find((f) => f.key === "platform")!;
    expect(platformField.required).toBe(true);
  });

  it("platform options include all expected platforms", () => {
    const platformField = pancakeConfig.find((f) => f.key === "platform")!;
    const values = platformField.options!.map((o) => o.value);
    expect(values).toContain("facebook");
    expect(values).toContain("instagram");
    expect(values).toContain("tiktok");
    expect(values).toContain("line");
    expect(values).toContain("shopee");
    expect(values).toContain("lazada");
    expect(values).toContain("tokopedia");
  });

  it("platform options do NOT include natively-supported channels", () => {
    const platformField = pancakeConfig.find((f) => f.key === "platform")!;
    const values = platformField.options!.map((o) => o.value);
    expect(values).not.toContain("telegram");
    expect(values).not.toContain("zalo");
    expect(values).not.toContain("whatsapp");
    expect(values).not.toContain("zalo_oa");
  });

  it("exposes private_reply feature toggle gated on fb/ig only", () => {
    const feat = pancakeConfig.find((f) => f.key === "features.private_reply");
    expect(feat).toBeDefined();
    expect(feat!.type).toBe("boolean");
    expect(feat!.defaultValue).toBe(false);
    expect(feat!.showWhen).toMatchObject({
      key: "platform",
      value: ["facebook", "instagram"],
    });
  });

  it("private_reply_mode has exactly after_reply + standalone options", () => {
    const mode = pancakeConfig.find((f) => f.key === "private_reply_mode")!;
    expect(mode.type).toBe("select");
    expect(mode.defaultValue).toBe("after_reply");
    const values = mode.options!.map((o) => o.value);
    expect(values).toEqual(["after_reply", "standalone"]);
  });

  it("private_reply fields are gated by features.private_reply", () => {
    const gated = [
      "private_reply_mode",
      "private_reply_message",
      "private_reply_ttl_days",
      "private_reply_options.allow_post_ids",
      "private_reply_options.deny_post_ids",
    ];
    for (const key of gated) {
      const f = pancakeConfig.find((x) => x.key === key);
      expect(f, `missing field ${key}`).toBeDefined();
      expect(f!.showWhen).toEqual({ key: "features.private_reply", value: "true" });
    }
  });

  it("private_reply_options.allow_post_ids is a tags field", () => {
    const allow = pancakeConfig.find((f) => f.key === "private_reply_options.allow_post_ids")!;
    expect(allow.type).toBe("tags");
  });

  it("private_reply_ttl_days defaults to 7", () => {
    const ttl = pancakeConfig.find((f) => f.key === "private_reply_ttl_days")!;
    expect(ttl.type).toBe("number");
    expect(ttl.defaultValue).toBe(7);
  });
});
