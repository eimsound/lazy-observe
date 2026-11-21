import {
  FileText,
  MousePointerBan,
  Palette,
  Rows3,
  Settings2,
  WrapText,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";

export interface DisplayFieldsMenuProps {
  fields: string[];
  displayFields: string[];
  singleLine: boolean;
  compactHeader: boolean;
  markdownParsing: boolean;
  ansiParsing: boolean;
  disableHover: boolean;
  onDisplayFieldsChange: (fields: string[]) => void;
  onSingleLineChange: (value: boolean) => void;
  onCompactHeaderChange: (value: boolean) => void;
  onMarkdownParsingChange: (value: boolean) => void;
  onAnsiParsingChange: (value: boolean) => void;
  onDisableHoverChange: (value: boolean) => void;
}

export function DisplayFieldsMenu({
  fields,
  displayFields,
  singleLine,
  compactHeader,
  markdownParsing,
  ansiParsing,
  disableHover,
  onDisplayFieldsChange,
  onSingleLineChange,
  onCompactHeaderChange,
  onMarkdownParsingChange,
  onAnsiParsingChange,
  onDisableHoverChange,
}: DisplayFieldsMenuProps) {
  const { t } = useTranslation();

  function toggleField(field: string, checked: boolean) {
    if (checked) {
      if (!displayFields.includes(field)) {
        onDisplayFieldsChange([...displayFields, field]);
      }
      return;
    }
    onDisplayFieldsChange(
      displayFields.filter((candidate) => candidate !== field),
    );
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button type="button" variant="outline" size="sm">
          <Settings2 />
          {t("logs.viewOptions")}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent
        align="end"
        className="max-h-[min(32rem,var(--radix-dropdown-menu-content-available-height))] w-64 overflow-y-auto"
      >
        <DropdownMenuLabel>{t("logs.displayFields")}</DropdownMenuLabel>
        {fields.map((field) => (
          <DropdownMenuCheckboxItem
            key={field}
            checked={displayFields.includes(field)}
            onCheckedChange={(checked) => toggleField(field, checked === true)}
            onSelect={(event) => event.preventDefault()}
          >
            <span className="truncate font-mono text-xs" title={field}>
              {field}
            </span>
          </DropdownMenuCheckboxItem>
        ))}
        <DropdownMenuSeparator />
        <DropdownMenuCheckboxItem
          checked={singleLine}
          onCheckedChange={(checked) => onSingleLineChange(checked === true)}
          onSelect={(event) => event.preventDefault()}
        >
          <WrapText className="mr-2 size-4" />
          {t("logs.singleLine")}
        </DropdownMenuCheckboxItem>
        <DropdownMenuCheckboxItem
          checked={compactHeader}
          onCheckedChange={(checked) => onCompactHeaderChange(checked === true)}
          onSelect={(event) => event.preventDefault()}
        >
          <Rows3 className="mr-2 size-4" />
          {t("logs.compactGroupHeader")}
        </DropdownMenuCheckboxItem>
        <DropdownMenuSeparator />
        <DropdownMenuLabel>{t("logs.messageRendering")}</DropdownMenuLabel>
        <DropdownMenuCheckboxItem
          checked={markdownParsing}
          onCheckedChange={(checked) =>
            onMarkdownParsingChange(checked === true)
          }
          onSelect={(event) => event.preventDefault()}
        >
          <FileText className="mr-2 size-4" />
          {t("logs.markdownParsing")}
        </DropdownMenuCheckboxItem>
        <DropdownMenuCheckboxItem
          checked={ansiParsing}
          onCheckedChange={(checked) => onAnsiParsingChange(checked === true)}
          onSelect={(event) => event.preventDefault()}
        >
          <Palette className="mr-2 size-4" />
          {t("logs.ansiParsing")}
        </DropdownMenuCheckboxItem>
        <DropdownMenuSeparator />
        <DropdownMenuCheckboxItem
          checked={disableHover}
          onCheckedChange={(checked) => onDisableHoverChange(checked === true)}
          onSelect={(event) => event.preventDefault()}
        >
          <MousePointerBan className="mr-2 size-4" />
          {t("logs.disableHoverEffects")}
        </DropdownMenuCheckboxItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
