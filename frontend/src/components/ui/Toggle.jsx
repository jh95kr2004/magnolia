import React from 'react';
import './Toggle.css';

export const Toggle = ({ checked, onChange, disabled }) => {
  return (
    <label className={`ui-toggle ${disabled ? 'opacity-50 cursor-not-allowed' : ''}`}>
      <input type="checkbox" checked={checked} onChange={onChange} disabled={disabled} />
      <span className="slider round"></span>
    </label>
  );
};
