import React, { useState, useRef, useEffect } from 'react';
import { HelpCircle } from 'lucide-react';

export const ColumnTooltipHeader = ({ label, tooltip, align = 'left' }) => {
  const [open, setOpen] = useState(false);
  const wrapRef = useRef(null);

  useEffect(() => {
    if (!open) return;
    const handleOutside = (e) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target)) {
        setOpen(false);
      }
    };
    document.addEventListener('mousedown', handleOutside);
    return () => document.removeEventListener('mousedown', handleOutside);
  }, [open]);

  if (!tooltip) {
    return <span>{label}</span>;
  }

  return (
    <span
      ref={wrapRef}
      className={`col-tooltip-wrap ${align === 'right' ? 'align-right' : 'align-left'}`}
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onClick={(e) => {
        e.stopPropagation();
        setOpen((prev) => !prev);
      }}
      data-testid="column-tooltip-trigger"
    >
      <span className="col-tooltip-label">{label}</span>
      <HelpCircle size={13} className="col-tooltip-icon" aria-hidden="true" />
      {open && (
        <span
          role="tooltip"
          data-testid="column-tooltip-popup"
          className={`col-tooltip-popup ${align === 'right' ? 'popup-right' : 'popup-left'}`}
        >
          {tooltip}
        </span>
      )}
    </span>
  );
};
