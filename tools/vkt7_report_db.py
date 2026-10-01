#!/usr/bin/env python3
# -*- coding: utf-8 -*-

"""
VKT-7 report/export from PostgreSQL.

Режимы:

1. PDF непосредственно из PostgreSQL:

    python vkt7_report_db.py pdf \
        --db-url "postgresql://vkt7:vkt7@127.0.0.1:5432/vkt7" \
        --device-id 4 \
        --month 2026-09 \
        --output report_2026-09.pdf

2. Экспорт CSV, совместимый с vkt7_export:

    python vkt7_report_db.py csv \
        --db-url "postgresql://vkt7:vkt7@127.0.0.1:5432/vkt7" \
        --device-id 4 \
        --month 2026-09 \
        --output-dir export

3. PDF без указания месяца — последний полный месяц:

    python vkt7_report_db.py pdf \
        --db-url "postgresql://vkt7:vkt7@127.0.0.1:5432/vkt7" \
        --device-id 4
"""

from __future__ import annotations

import argparse
import calendar
import json
import logging
import os
import sys
from dataclasses import dataclass
from datetime import date, datetime
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Tuple

import psycopg2
from psycopg2.extras import RealDictCursor

from reportlab.lib import colors
from reportlab.lib.enums import TA_CENTER
from reportlab.lib.pagesizes import A4, landscape
from reportlab.lib.styles import ParagraphStyle, getSampleStyleSheet
from reportlab.lib.units import mm
from reportlab.platypus import (
    Paragraph,
    SimpleDocTemplate,
    Table,
    TableStyle,
    PageBreak,
)


LOG = logging.getLogger("vkt7_report_db")


# ============================================================================
# ПАРАМЕТРЫ VKT-7
# ============================================================================

# ID элементов соответствуют vkt7_export.py.
PARAMETERS: Dict[int, Tuple[str, str, Optional[str]]] = {
    # ТВ1
    0: ("t1", "TV1", "t"),
    1: ("t2", "TV1", "t"),
    2: ("t3", "TV1", "t"),
    3: ("V1", "TV1", "V1"),
    4: ("V2", "TV1", "V1"),
    5: ("V3", "TV1", "V1"),
    6: ("M1", "TV1", "M1"),
    7: ("M2", "TV1", "M1"),
    8: ("M3", "TV1", "M1"),
    9: ("P1", "TV1", "P"),
    10: ("P2", "TV1", "P"),
    11: ("Mg", "TV1", "M1"),
    12: ("Qo", "TV1", "Q1"),
    13: ("Qg", "TV1", "Q1"),
    14: ("dt", "TV1", "t"),
    15: ("tx", "COMMON", "t"),
    16: ("ta", "COMMON", "t"),
    17: ("BNP", "TV1", None),
    18: ("VOC", "TV1", None),
    19: ("G1", "TV1", None),
    20: ("G2", "TV1", None),
    21: ("G3", "TV1", None),

    # ТВ2
    22: ("t1", "TV2", "t"),
    23: ("t2", "TV2", "t"),
    24: ("t3", "TV2", "t"),
    25: ("V1", "TV2", "V2"),
    26: ("V2", "TV2", "V2"),
    27: ("V3", "TV2", "V2"),
    28: ("M1", "TV2", "M2"),
    29: ("M2", "TV2", "M2"),
    30: ("M3", "TV2", "M2"),
    31: ("P1", "TV2", "P"),
    32: ("P2", "TV2", "P"),
    33: ("Mg", "TV2", "M2"),
    34: ("Qo", "TV2", "Q2"),
    35: ("Qg", "TV2", "Q2"),
    36: ("dt", "TV2", "t"),
    37: ("tx", "TV2", "t"),
    38: ("ta", "TV2", "t"),
    39: ("BNP", "TV2", None),
    40: ("VOC", "TV2", None),
    41: ("G1", "TV2", None),
    42: ("G2", "TV2", None),
    43: ("G3", "TV2", None),

    # Нештатные ситуации
    77: ("NS_present", "TV1", "char"),
    78: ("NS_present", "TV2", "char"),
    79: ("NS_duration", "TV1", "array10"),
    80: ("NS_duration", "TV2", "array10"),

    # COMMON
    81: ("DI", "COMMON", None),
    82: ("P3", "COMMON", "P"),
}


ID_BY_NAME: Dict[str, int] = {}

for _id, (_name, _circuit, _scale) in PARAMETERS.items():
    # Для ТВ1/ТВ2 одинаковые имена допустимы.
    # Для COMMON имя уникально.
    ID_BY_NAME[f"{_circuit}:{_name}"] = _id


# Поля, которые реально используются старым PDF-шаблоном.
TV1_REPORT_FIELDS = [
    "Qo",
    "M1",
    "M2",
    "t1",
    "t2",
    "dt",
    "P1",
    "P2",
]

TV2_REPORT_FIELDS = [
    "Qo",
    "V1",
    "t1",
    "BNP",
]


# ============================================================================
# УТИЛИТЫ ДАТ
# ============================================================================

def parse_month(value: Optional[str]) -> Tuple[date, date]:
    """
    Возвращает [first_day, first_day_of_next_month].

    Если month=None, используется предыдущий полный календарный месяц.
    """
    if value:
        try:
            year, month = map(int, value.split("-"))
            if not 1 <= month <= 12:
                raise ValueError
            first = date(year, month, 1)
        except ValueError:
            raise SystemExit(
                f"Неверный месяц: {value!r}. Используйте YYYY-MM."
            )
    else:
        today = date.today()

        if today.month == 1:
            year = today.year - 1
            month = 12
        else:
            year = today.year
            month = today.month - 1

        first = date(year, month, 1)

    if first.month == 12:
        next_month = date(first.year + 1, 1, 1)
    else:
        next_month = date(first.year, first.month + 1, 1)

    return first, next_month


def month_title(first_day: date) -> str:
    months = [
        "",
        "январь",
        "февраль",
        "март",
        "апрель",
        "май",
        "июнь",
        "июль",
        "август",
        "сентябрь",
        "октябрь",
        "ноябрь",
        "декабрь",
    ]
    return f"{months[first_day.month]} {first_day.year}"


# ============================================================================
# JSON / VKT-7 VALUES
# ============================================================================

def normalize_json(value: Any) -> Any:
    """
    PostgreSQL JSONB обычно приходит уже как dict/list.

    Функция дополнительно обрабатывает случай, когда драйвер/старый
    вариант БД вернул JSON в виде строки.
    """
    if value is None:
        return None

    if isinstance(value, (dict, list, int, float, bool)):
        return value

    if isinstance(value, Decimal):
        return float(value)

    if isinstance(value, str):
        s = value.strip()

        if not s:
            return None

        try:
            return json.loads(s)
        except Exception:
            return value

    return value


def json_get(obj: Any, key: str) -> Any:
    """
    Безопасное получение значения из JSON.

    Поддерживаются несколько вариантов хранения:
      {"12": 123.45}
      {12: 123.45}
      {"Qo": 123.45}
      {"TV1": {"Qo": 123.45}}
      {"TV1:Qo": 123.45}

    Это специально сделано для совместимости с текущей JSONB-моделью.
    """

    obj = normalize_json(obj)

    if not isinstance(obj, dict):
        return None

    # Прямой ключ.
    if key in obj:
        return obj[key]

    # Строковое/числовое представление.
    try:
        if str(key) in obj:
            return obj[str(key)]
    except Exception:
        pass

    return None


def extract_element(values: Any, element_id: int) -> Any:
    """
    Получение значения элемента VKT-7 из JSONB.

    Основной формат vkt7d ожидается как:
        {"12": value}

    Дополнительно поддерживаем:
        {"Qo": value}
        {"TV1": {"Qo": value}}
        {"TV1:Qo": value}
    """

    values = normalize_json(values)

    if not isinstance(values, dict):
        return None

    name, circuit, _scale = PARAMETERS.get(
        element_id,
        (None, None, None),
    )

    candidates = [
        str(element_id),
        element_id,
    ]

    if name:
        candidates.extend([
            name,
            f"{circuit}:{name}",
            f"{circuit}.{name}",
        ])

    for candidate in candidates:
        if candidate in values:
            item = values[candidate]

            # Некоторые реализации могут хранить:
            # {"12": {"value": 123, ...}}
            if isinstance(item, dict):
                for k in ("value", "raw_value", "numeric_value"):
                    if k in item:
                        return item[k]

            return item

    # Вложенный TV1/TV2.
    if circuit and circuit in values:
        nested = values[circuit]

        if isinstance(nested, dict):
            for candidate in candidates:
                if candidate in nested:
                    item = nested[candidate]

                    if isinstance(item, dict):
                        for k in ("value", "raw_value", "numeric_value"):
                            if k in item:
                                return item[k]

                    return item

    return None


def element_id_for(circuit: str, name: str) -> Optional[int]:
    return ID_BY_NAME.get(f"{circuit}:{name}")


def value_from_record(
    record: Dict[str, Any],
    circuit: str,
    name: str,
) -> Any:
    element_id = element_id_for(circuit, name)

    if element_id is None:
        return None

    return extract_element(record.get("values"), element_id)


def to_number(value: Any) -> Optional[float]:
    if value is None:
        return None

    if isinstance(value, bool):
        return float(value)

    if isinstance(value, (int, float)):
        return float(value)

    if isinstance(value, Decimal):
        return float(value)

    if isinstance(value, str):
        s = value.strip()

        if not s:
            return None

        # Допускаем русскую десятичную запятую.
        s = s.replace(",", ".")

        try:
            return float(s)
        except ValueError:
            return None

    if isinstance(value, dict):
        for key in ("value", "numeric_value", "raw_value"):
            if key in value:
                return to_number(value[key])

    return None


def format_csv_value(value: Any) -> str:
    """
    Формат максимально близкий к vkt7_export.py:
    float -> .15g
    list -> элементы через ;
    None -> пустая строка
    """

    if value is None:
        return ""

    if isinstance(value, float):
        return format(value, ".15g")

    if isinstance(value, Decimal):
        return format(float(value), ".15g")

    if isinstance(value, list):
        return ";".join(str(x) for x in value)

    return str(value)


# ============================================================================
# POSTGRESQL
# ============================================================================

def connect_db(db_url: str):
    conn = psycopg2.connect(db_url)
    conn.autocommit = False
    return conn


def get_device(
    conn,
    device_id: int,
) -> Dict[str, Any]:
    sql = """
        SELECT
            id,
            name,
            address,
            serial_port,
            baud_rate,
            firmware_version,
            scheme_tv1,
            scheme_tv2,
            subscriber_id,
            report_day,
            model,
            active_db,
            last_seen_at
        FROM vkt7.devices
        WHERE id = %s
    """

    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(sql, (device_id,))
        row = cur.fetchone()

    if not row:
        raise RuntimeError(
            f"Устройство device_id={device_id} не найдено "
            f"в vkt7.devices."
        )

    return dict(row)


def get_daily_rows(
    conn,
    device_id: int,
    first_day: date,
    next_month: date,
) -> List[Dict[str, Any]]:
    """
    Получает весь суточный архив за календарный месяц.

    LEFT/INNER JOIN здесь не нужен: в одной записи daily_archive
    могут присутствовать элементы ТВ1 и ТВ2.
    """

    sql = """
        SELECT
            device_id,
            archive_date,
            scheme_tv1,
            scheme_tv2,
            active_db,
            "values",
            quality,
            ns,
            raw,
            collected_at
        FROM vkt7.daily_archive
        WHERE device_id = %s
          AND archive_date >= %s
          AND archive_date < %s
        ORDER BY archive_date
    """

    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute(
            sql,
            (
                device_id,
                first_day,
                next_month,
            ),
        )

        return [dict(row) for row in cur.fetchall()]


# ============================================================================
# ПРЕОБРАЗОВАНИЕ БД -> CSV RECORDS
# ============================================================================

def db_row_to_tv_record(
    row: Dict[str, Any],
    circuit: str,
) -> Dict[str, Any]:
    """
    Делает логическую запись в формате старого CSV exporter.

    Для ТВ1 и ТВ2 используется полный набор PARAMETERS.
    """

    result: Dict[str, Any] = {
        "date": row["archive_date"].isoformat()
        if isinstance(row["archive_date"], date)
        else str(row["archive_date"])
    }

    for element_id, (
        name,
        element_circuit,
        _scale,
    ) in PARAMETERS.items():

        if element_circuit != circuit:
            continue

        result[name] = extract_element(
            row.get("values"),
            element_id,
        )

    return result


def build_records(
    rows: Iterable[Dict[str, Any]],
) -> Tuple[List[Dict[str, Any]], List[Dict[str, Any]]]:

    tv1_records = []
    tv2_records = []

    for row in rows:
        tv1_records.append(
            db_row_to_tv_record(row, "TV1")
        )

        tv2_records.append(
            db_row_to_tv_record(row, "TV2")
        )

    return tv1_records, tv2_records


# ============================================================================
# CSV
# ============================================================================

CSV_COLUMNS = [
    "date",

    "t1",
    "t2",
    "t3",

    "V1",
    "V2",
    "V3",

    "M1",
    "M2",
    "M3",

    "P1",
    "P2",

    "Mg",
    "Qo",
    "Qg",
    "dt",
    "tx",
    "ta",
    "BNP",
    "VOC",
    "G1",
    "G2",
    "G3",

    "NS_present",
    "NS_duration",

    "DI",
    "P3",
]


def write_csv(
    records: List[Dict[str, Any]],
    output: Path,
) -> None:

    import csv

    output.parent.mkdir(
        parents=True,
        exist_ok=True,
    )

    with output.open(
        "w",
        encoding="utf-8-sig",
        newline="",
    ) as f:

        writer = csv.DictWriter(
            f,
            fieldnames=CSV_COLUMNS,
            delimiter=";",
            extrasaction="ignore",
        )

        writer.writeheader()

        for record in records:
            writer.writerow({
                key: format_csv_value(
                    record.get(key)
                )
                for key in CSV_COLUMNS
            })


def export_csv(
    conn,
    device_id: int,
    first_day: date,
    next_month: date,
    output_dir: Path,
) -> None:

    rows = get_daily_rows(
        conn,
        device_id,
        first_day,
        next_month,
    )

    tv1, tv2 = build_records(rows)

    output_dir.mkdir(
        parents=True,
        exist_ok=True,
    )

    write_csv(
        tv1,
        output_dir / "tv1.csv",
    )

    write_csv(
        tv2,
        output_dir / "tv2.csv",
    )

    LOG.info(
        "Экспортировано: %d суточных записей",
        len(rows),
    )

    LOG.info(
        "TV1: %s",
        output_dir / "tv1.csv",
    )

    LOG.info(
        "TV2: %s",
        output_dir / "tv2.csv",
    )


# ============================================================================
# PDF
# ============================================================================

def format_number(value: Any, decimals: int = 3) -> str:
    number = to_number(value)

    if number is None:
        return ""

    # Не показываем -0.000.
    if abs(number) < 0.5 * 10 ** (-decimals):
        number = 0.0

    return f"{number:.{decimals}f}"


def sum_values(values: Iterable[Any]) -> Optional[float]:
    numbers = []

    for value in values:
        number = to_number(value)

        if number is not None:
            numbers.append(number)

    if not numbers:
        return None

    return sum(numbers)


def average_values(values: Iterable[Any]) -> Optional[float]:
    numbers = []

    for value in values:
        number = to_number(value)

        if number is not None:
            numbers.append(number)

    if not numbers:
        return None

    return sum(numbers) / len(numbers)


def paragraph(
    value: Any,
    style: ParagraphStyle,
) -> Paragraph:

    if value is None:
        value = ""

    return Paragraph(
        str(value),
        style,
    )


def find_font() -> str:
    """
    Ищем шрифт с кириллицей.

    Приоритет:
      1. DejaVuSans.ttf рядом со скриптом
      2. системные DejaVu Sans
      3. Liberation Sans

    Если шрифт не найден — Helvetica.
    """

    from reportlab.pdfbase import pdfmetrics
    from reportlab.pdfbase.ttfonts import TTFont

    script_dir = Path(__file__).resolve().parent

    candidates = [
        script_dir / "DejaVuSans.ttf",

        Path("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"),
        Path("/usr/share/fonts/truetype/dejavu/DejaVuSansCondensed.ttf"),

        Path(
            "/usr/share/fonts/truetype/liberation2/"
            "LiberationSans-Regular.ttf"
        ),

        Path(
            "/usr/share/fonts/truetype/liberation/"
            "LiberationSans-Regular.ttf"
        ),
    ]

    for path in candidates:
        if not path.exists():
            continue

        try:
            pdfmetrics.registerFont(
                TTFont(
                    "VKT7Font",
                    str(path),
                )
            )

            return "VKT7Font"

        except Exception as exc:
            LOG.warning(
                "Не удалось загрузить шрифт %s: %s",
                path,
                exc,
            )

    LOG.warning(
        "Шрифт с кириллицей не найден; "
        "текст PDF может отображаться некорректно."
    )

    return "Helvetica"


def create_pdf(
    rows: List[Dict[str, Any]],
    device: Dict[str, Any],
    first_day: date,
    next_month: date,
    output: Path,
) -> None:

    FONT_NAME = find_font()

    styles = getSampleStyleSheet()

    title_style = ParagraphStyle(
        "VKTTitle",
        parent=styles["Title"],
        fontName=FONT_NAME,
        fontSize=14,
        leading=16,
        alignment=TA_CENTER,
        spaceAfter=5 * mm,
    )

    header_style = ParagraphStyle(
        "VKTHeader",
        parent=styles["Normal"],
        fontName=FONT_NAME,
        fontSize=7,
        leading=8,
        alignment=TA_CENTER,
    )

    cell_style = ParagraphStyle(
        "VKTCell",
        parent=styles["Normal"],
        fontName=FONT_NAME,
        fontSize=7,
        leading=8,
        alignment=TA_CENTER,
    )

    total_style = ParagraphStyle(
        "VKTTotal",
        parent=cell_style,
        fontName=FONT_NAME,
        fontSize=7,
        leading=8,
        alignment=TA_CENTER,
    )

    doc = SimpleDocTemplate(
        str(output),
        pagesize=landscape(A4),
        rightMargin=8 * mm,
        leftMargin=8 * mm,
        topMargin=8 * mm,
        bottomMargin=8 * mm,
        title="Архив ВКТ-7",
        author="Python / PostgreSQL",
    )

    elements = []

    # ------------------------------------------------------------------------
    # Мапы по датам
    # ------------------------------------------------------------------------

    rows_by_date: Dict[date, Dict[str, Any]] = {}

    for row in rows:
        d = row["archive_date"]

        if isinstance(d, datetime):
            d = d.date()

        if isinstance(d, str):
            d = date.fromisoformat(d)

        rows_by_date[d] = row

    all_dates = sorted(rows_by_date)

    # ------------------------------------------------------------------------
    # СТРАНИЦА 1
    # ------------------------------------------------------------------------

    elements.append(
        Paragraph(
            "Название организации и номер договора",
            title_style,
        )
    )

    table_data = []

    # Верхний уровень.
    table_data.append([
        paragraph(
            "<b>Отчет о суточных параметрах "
            "теплопотребления за период</b>",
            header_style,
        ),
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
    ])

    # Средний уровень.
    table_data.append([
        paragraph("Дата", header_style),

        paragraph("Отопление", header_style),
        "",
        "",
        "",
        "",
        "",
        "",
        "",

        paragraph("Горячая вода", header_style),
        "",
        "",

        paragraph(
            "Период нормальной работы, ч",
            header_style,
        ),
    ])

    # Нижний уровень.
    table_data.append([
        "",

        paragraph(
            "Qотопления, Гкал",
            header_style,
        ),
        paragraph("Mпод, т", header_style),
        paragraph("Mобр, т", header_style),
        paragraph("Tпод, °C", header_style),
        paragraph("Tобр, °C", header_style),
        paragraph("ΔT, °C", header_style),
        paragraph("Pпод, кг/см²", header_style),
        paragraph("Pобр, кг/см²", header_style),

        paragraph("Qгвс, Гкал", header_style),
        paragraph("Mгвс, м³", header_style),
        paragraph("Tгвс, °C", header_style),

        "",
    ])

    daily_rows = []

    for d in all_dates:
        row = rows_by_date[d]

        tv1 = [
            value_from_record(row, "TV1", "Qo"),
            value_from_record(row, "TV1", "M1"),
            value_from_record(row, "TV1", "M2"),
            value_from_record(row, "TV1", "t1"),
            value_from_record(row, "TV1", "t2"),
            value_from_record(row, "TV1", "dt"),
            value_from_record(row, "TV1", "P1"),
            value_from_record(row, "TV1", "P2"),
        ]

        tv2 = [
            value_from_record(row, "TV2", "Qo"),
            value_from_record(row, "TV2", "V1"),
            value_from_record(row, "TV2", "t1"),
            value_from_record(row, "TV2", "BNP"),
        ]

        row_values = tv1 + tv2

        daily_rows.append(row_values)

        date_string = d.strftime("%d.%m.%Y")

        table_data.append([
            paragraph(
                date_string,
                cell_style,
            )
        ] + [
            paragraph(
                format_number(value),
                cell_style,
            )
            for value in row_values
        ] + [
            paragraph(
                format_number(
                    value_from_record(
                        row,
                        "TV1",
                        "BNP",
                    )
                ),
                cell_style,
            )
        ])

    # ------------------------------------------------------------------------
    # ИТОГО
    # ------------------------------------------------------------------------

    sum_heating_qo = sum_values(
        row[0]
        for row in daily_rows
    )

    sum_heating_m1 = sum_values(
        row[1]
        for row in daily_rows
    )

    sum_heating_m2 = sum_values(
        row[2]
        for row in daily_rows
    )

    sum_hotwater_qo = sum_values(
        row[8]
        for row in daily_rows
    )

    sum_hotwater_v1 = sum_values(
        row[9]
        for row in daily_rows
    )

    sum_hotwater_bnp = sum_values(
        row[11]
        for row in daily_rows
    )

    total_row = [
        paragraph(
            "<b>Итого</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(sum_heating_qo)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(sum_heating_m1)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(sum_heating_m2)}</b>",
            total_style,
        ),

        paragraph("", total_style),
        "",
        "",
        "",
        "",

        paragraph(
            f"<b>{format_number(sum_hotwater_qo)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(sum_hotwater_v1)}</b>",
            total_style,
        ),

        "",

        paragraph(
            f"<b>{format_number(sum_hotwater_bnp)}</b>",
            total_style,
        ),
    ]

    table_data.append(total_row)

    total_row_index = len(table_data) - 1

    # ------------------------------------------------------------------------
    # Средние значения
    # ------------------------------------------------------------------------

    avg_heating_t1 = average_values(
        row[3]
        for row in daily_rows
    )

    avg_heating_t2 = average_values(
        row[4]
        for row in daily_rows
    )

    avg_heating_dt = average_values(
        row[5]
        for row in daily_rows
    )

    avg_heating_p1 = average_values(
        row[6]
        for row in daily_rows
    )

    avg_heating_p2 = average_values(
        row[7]
        for row in daily_rows
    )

    avg_hotwater_t1 = average_values(
        row[10]
        for row in daily_rows
    )

    average_row = [
        paragraph(
            "<b>Среднее</b>",
            total_style,
        ),

        "",
        "",
        "",

        paragraph(
            f"<b>{format_number(avg_heating_t1)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(avg_heating_t2)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(avg_heating_dt)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(avg_heating_p1)}</b>",
            total_style,
        ),

        paragraph(
            f"<b>{format_number(avg_heating_p2)}</b>",
            total_style,
        ),

        "",

        "",

        paragraph(
            f"<b>{format_number(avg_hotwater_t1)}</b>",
            total_style,
        ),

        "",
    ]

    table_data.append(average_row)

    average_row_index = len(table_data) - 1

    # ------------------------------------------------------------------------
    # Ширины таблицы
    # ------------------------------------------------------------------------

    col_widths = [
        27 * mm,

        19 * mm,
        19 * mm,
        19 * mm,
        18 * mm,
        18 * mm,
        18 * mm,
        18 * mm,
        18 * mm,

        19 * mm,
        19 * mm,
        18 * mm,

        20 * mm,
    ]

    table = Table(
        table_data,
        colWidths=col_widths,
        repeatRows=3,
        hAlign="CENTER",
    )

    table.setStyle(
        TableStyle([
            (
                "GRID",
                (0, 0),
                (-1, -1),
                0.4,
                colors.black,
            ),
            (
                "VALIGN",
                (0, 0),
                (-1, -1),
                "MIDDLE",
            ),
            (
                "ALIGN",
                (0, 0),
                (-1, -1),
                "CENTER",
            ),
            (
                "BACKGROUND",
                (0, 0),
                (-1, 2),
                colors.lightgrey,
            ),
            (
                "FONTNAME",
                (0, 0),
                (-1, -1),
                FONT_NAME,
            ),

            # Заголовок всего отчёта.
            (
                "SPAN",
                (0, 0),
                (12, 0),
            ),

            # Отопление.
            (
                "SPAN",
                (1, 1),
                (8, 1),
            ),

            # Горячая вода.
            (
                "SPAN",
                (9, 1),
                (11, 1),
            ),

            # Дата.
            (
                "SPAN",
                (0, 1),
                (0, 2),
            ),

            # BNP.
            (
                "SPAN",
                (12, 1),
                (12, 2),
            ),

            # Итого.
            (
                "SPAN",
                (4, total_row_index),
                (8, total_row_index),
            ),

            (
                "LINEABOVE",
                (0, total_row_index),
                (-1, total_row_index),
                1.0,
                colors.black,
            ),

            (
                "LINEABOVE",
                (0, average_row_index),
                (-1, average_row_index),
                1.0,
                colors.black,
            ),

            (
                "TOPPADDING",
                (0, 0),
                (-1, 1),
                4,
            ),

            (
                "BOTTOMPADDING",
                (0, 0),
                (-1, 1),
                4,
            ),

            (
                "TOPPADDING",
                (0, total_row_index),
                (-1, average_row_index),
                4,
            ),

            (
                "BOTTOMPADDING",
                (0, total_row_index),
                (-1, average_row_index),
                4,
            ),
        ])
    )

    elements.append(table)

    # ------------------------------------------------------------------------
    # СТРАНИЦА 2 — ХОЛОДНАЯ ВОДА
    # ------------------------------------------------------------------------

    elements.append(PageBreak())

    elements.append(
        Paragraph(
            "Архив ВКТ-7 — холодная вода",
            title_style,
        )
    )

    cold_table_data = [
        [
            paragraph("Дата", header_style),
            paragraph("V3", header_style),
            paragraph("BNP", header_style),
        ]
    ]

    cold_rows = []

    for d in all_dates:
        row = rows_by_date[d]

        v3 = value_from_record(
            row,
            "TV2",
            "V3",
        )

        bnp = value_from_record(
            row,
            "TV2",
            "BNP",
        )

        cold_rows.append([
            v3,
            bnp,
        ])

        cold_table_data.append([
            paragraph(
                d.strftime("%d.%m.%Y"),
                cell_style,
            ),
            paragraph(
                format_number(v3),
                cell_style,
            ),
            paragraph(
                format_number(bnp),
                cell_style,
            ),
        ])

    cold_total_v3 = sum_values(
        row[0]
        for row in cold_rows
    )

    cold_total_bnp = sum_values(
        row[1]
        for row in cold_rows
    )

    cold_total_row = [
        paragraph(
            "<b>Итого</b>",
            total_style,
        ),
        paragraph(
            f"<b>{format_number(cold_total_v3)}</b>",
            total_style,
        ),
        paragraph(
            f"<b>{format_number(cold_total_bnp)}</b>",
            total_style,
        ),
    ]

    cold_table_data.append(cold_total_row)

    cold_total_row_index = len(cold_table_data) - 1

    cold_table = Table(
        cold_table_data,
        colWidths=[
            45 * mm,
            45 * mm,
            45 * mm,
        ],
        repeatRows=1,
        hAlign="CENTER",
    )

    cold_table.setStyle(
        TableStyle([
            (
                "GRID",
                (0, 0),
                (-1, -1),
                0.5,
                colors.black,
            ),
            (
                "VALIGN",
                (0, 0),
                (-1, -1),
                "MIDDLE",
            ),
            (
                "ALIGN",
                (0, 0),
                (-1, -1),
                "CENTER",
            ),
            (
                "BACKGROUND",
                (0, 0),
                (-1, 0),
                colors.lightgrey,
            ),
            (
                "FONTNAME",
                (0, 0),
                (-1, -1),
                FONT_NAME,
            ),
            (
                "LINEABOVE",
                (0, cold_total_row_index),
                (-1, cold_total_row_index),
                1.0,
                colors.black,
            ),
            (
                "TOPPADDING",
                (0, 0),
                (-1, 0),
                5,
            ),
            (
                "BOTTOMPADDING",
                (0, 0),
                (-1, 0),
                5,
            ),
            (
                "TOPPADDING",
                (0, cold_total_row_index),
                (-1, cold_total_row_index),
                5,
            ),
            (
                "BOTTOMPADDING",
                (0, cold_total_row_index),
                (-1, cold_total_row_index),
                5,
            ),
        ])
    )

    elements.append(cold_table)

    # ------------------------------------------------------------------------
    # Информация об устройстве — мелким текстом внизу.
    # ------------------------------------------------------------------------

    device_info = (
        f"Устройство: {device.get('name', '')}; "
        f"адрес: {device.get('address', '')}; "
        f"device_id: {device.get('id', '')}; "
        f"период: {first_day.strftime('%d.%m.%Y')} — "
        f"{(next_month.fromordinal(next_month.toordinal() - 1)).strftime('%d.%m.%Y')}"
    )

    info_style = ParagraphStyle(
        "VKTInfo",
        parent=styles["Normal"],
        fontName=FONT_NAME,
        fontSize=6,
        leading=7,
        alignment=TA_CENTER,
        spaceBefore=4 * mm,
    )

    elements.append(
        Paragraph(
            device_info,
            info_style,
        )
    )

    doc.build(elements)


# ============================================================================
# CLI
# ============================================================================

def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=(
            "Экспорт суточного архива ВКТ-7 из PostgreSQL "
            "и генерация PDF без промежуточного CSV."
        )
    )

    parser.add_argument(
        "command",
        choices=[
            "pdf",
            "csv",
        ],
    )

    parser.add_argument(
        "--db-url",
        default=os.environ.get(
            "VKT7_DB_URL",
            "postgresql://vkt7:vkt7@127.0.0.1:5432/vkt7",
        ),
        help="PostgreSQL connection URL.",
    )

    parser.add_argument(
        "--device-id",
        type=int,
        default=None,
        help="ID устройства из vkt7.devices.",
    )

    parser.add_argument(
        "--month",
        default=None,
        help="Месяц YYYY-MM. По умолчанию последний полный месяц.",
    )

    parser.add_argument(
        "--output",
        default=None,
        help="PDF output file.",
    )

    parser.add_argument(
        "--output-dir",
        default="export",
        help="Каталог для CSV.",
    )

    parser.add_argument(
        "--verbose",
        action="store_true",
    )

    return parser


def resolve_device_id(
    conn,
    requested: Optional[int],
) -> int:

    if requested is not None:
        # Проверяем наличие.
        get_device(conn, requested)
        return requested

    sql = """
        SELECT id
        FROM vkt7.devices
        ORDER BY id
        LIMIT 1
    """

    with conn.cursor() as cur:
        cur.execute(sql)
        row = cur.fetchone()

    if not row:
        raise RuntimeError(
            "В vkt7.devices нет устройств."
        )

    return int(row[0])


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()

    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
    )

    first_day, next_month = parse_month(
        args.month
    )

    LOG.info(
        "Период: %s — %s",
        first_day,
        next_month,
    )

    conn = None

    try:
        conn = connect_db(args.db_url)

        device_id = resolve_device_id(
            conn,
            args.device_id,
        )

        device = get_device(
            conn,
            device_id,
        )

        LOG.info(
            "Устройство: id=%s name=%s address=%s",
            device["id"],
            device["name"],
            device["address"],
        )

        rows = get_daily_rows(
            conn,
            device_id,
            first_day,
            next_month,
        )

        LOG.info(
            "Найдено суточных записей: %d",
            len(rows),
        )

        if args.command == "csv":
            output_dir = Path(args.output_dir)

            tv1, tv2 = build_records(rows)

            output_dir.mkdir(
                parents=True,
                exist_ok=True,
            )

            write_csv(
                tv1,
                output_dir / "tv1.csv",
            )

            write_csv(
                tv2,
                output_dir / "tv2.csv",
            )

            print(
                f"TV1: {output_dir / 'tv1.csv'}"
            )

            print(
                f"TV2: {output_dir / 'tv2.csv'}"
            )

        elif args.command == "pdf":
            if args.output:
                output = Path(args.output)
            else:
                output = Path(
                    f"vkt7_{device['name']}_"
                    f"{first_day.strftime('%Y-%m')}.pdf"
                )

            output.parent.mkdir(
                parents=True,
                exist_ok=True,
            )

            create_pdf(
                rows,
                device,
                first_day,
                next_month,
                output,
            )

            print(
                f"PDF: {output}"
            )

        conn.commit()
        return 0

    except KeyboardInterrupt:
        LOG.warning("Остановлено пользователем.")
        return 130

    except Exception as exc:
        if conn is not None:
            conn.rollback()

        LOG.exception(
            "Ошибка: %s",
            exc,
        )

        return 1

    finally:
        if conn is not None:
            conn.close()


if __name__ == "__main__":
    sys.exit(main())