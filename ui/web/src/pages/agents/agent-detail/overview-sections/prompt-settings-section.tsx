import { useState, useEffect } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { toast } from "@/stores/use-toast-store";
import type { AgentData } from "@/types/agent";
import { readPromptMode } from "../agent-display-utils";
import { PromptModeCards, type PromptMode } from "../../prompt-mode-cards";
import { VoicePicker } from "@/components/voice-picker";

interface Props {
  agent: AgentData;
  onUpdate: (updates: Record<string, unknown>) => Promise<void>;
}

const TTS_MODELS = [
  { value: "eleven_v3", label: "Eleven v3" },
  { value: "eleven_flash_v2_5", label: "Eleven Flash v2.5" },
  { value: "eleven_multilingual_v2", label: "Eleven Multilingual v2" },
  { value: "eleven_turbo_v2_5", label: "Eleven Turbo v2.5" },
] as const;

export function PromptSettingsSection({ agent, onUpdate }: Props) {
  const { t } = useTranslation("agents");
  const { t: tTts } = useTranslation("tts");

  const otherConfig = (agent.other_config ?? {}) as Record<string, unknown>;
  const savedMode = readPromptMode(agent) as PromptMode;

  const [mode, setMode] = useState<PromptMode>(savedMode);
  const [ttsVoiceId, setTtsVoiceId] = useState<string>((otherConfig.tts_voice_id as string) ?? "");
  const [ttsModelId, setTtsModelId] = useState<string>((otherConfig.tts_model_id as string) ?? "");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    const cfg = (agent.other_config ?? {}) as Record<string, unknown>;
    setMode(readPromptMode(agent) as PromptMode);
    setTtsVoiceId((cfg.tts_voice_id as string) ?? "");
    setTtsModelId((cfg.tts_model_id as string) ?? "");
  }, [agent.other_config]);

  const savedVoiceId = (otherConfig.tts_voice_id as string) ?? "";
  const savedModelId = (otherConfig.tts_model_id as string) ?? "";
  const dirty = mode !== savedMode || ttsVoiceId !== savedVoiceId || ttsModelId !== savedModelId;

  const handleSave = async () => {
    setSaving(true);
    try {
      const bag = { ...otherConfig };
      if (mode && mode !== "full") {
        bag.prompt_mode = mode;
      } else {
        delete bag.prompt_mode;
      }
      if (ttsVoiceId) {
        bag.tts_voice_id = ttsVoiceId;
      } else {
        delete bag.tts_voice_id;
      }
      if (ttsModelId) {
        bag.tts_model_id = ttsModelId;
      } else {
        delete bag.tts_model_id;
      }
      await onUpdate({ other_config: bag });
      const modeRank: Record<string, number> = { none: 0, minimal: 1, task: 2, full: 3 };
      if ((modeRank[mode] ?? 3) > (modeRank[savedMode] ?? 3)) {
        toast.info(t("detail.prompt.upgradeWarning", "Mode upgraded. Some files may need regeneration — use Resummon or Edit with AI in the Files tab."));
      }
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="space-y-3 rounded-lg border p-3 sm:p-4">
      <div className="flex items-center justify-between">
        <h3 className="text-sm font-medium">{t("detail.prompt.title")}</h3>
        {dirty && (
          <Button size="sm" onClick={handleSave} disabled={saving}>
            {saving ? t("saving", "Saving...") : t("save", "Save")}
          </Button>
        )}
      </div>

      <PromptModeCards value={mode} onChange={setMode} />

      {/* TTS subsection */}
      <div className="space-y-3 border-t pt-3">
        <h4 className="text-xs font-medium text-muted-foreground uppercase tracking-wide">
          {tTts("title")}
        </h4>
        <div className="space-y-2">
          <Label className="text-sm">{tTts("voice_label")}</Label>
          <VoicePicker
            value={ttsVoiceId || undefined}
            onChange={setTtsVoiceId}
          />
        </div>
        <div className="space-y-2">
          <Label className="text-sm">{tTts("model_label")}</Label>
          <Select value={ttsModelId} onValueChange={setTtsModelId}>
            <SelectTrigger className="w-full text-base md:text-sm">
              <SelectValue placeholder={tTts("model_placeholder")} />
            </SelectTrigger>
            <SelectContent>
              {TTS_MODELS.map((m) => (
                <SelectItem key={m.value} value={m.value}>
                  {m.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </div>
    </section>
  );
}
